package appbuild

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

func imageTestCA(t *testing.T, ca bool) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Image Test"}, NotBefore: time.Unix(0, 0), NotAfter: time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC), BasicConstraintsValid: true, IsCA: ca, KeyUsage: x509.KeyUsageCertSign}
	raw, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: raw})
}

func imageFixture(t *testing.T, ca []byte) (string, *ImageRecord) {
	t.Helper()
	root := t.TempDir()
	application := []byte("fixed application bytes")
	put(t, root, "application", string(application))
	put(t, root, "trust.pem", string(ca))
	native := fixtureRecord()
	record := &ImageRecord{Format: 1, PackerCompiler: native.BuildCompiler, PackerCompilerArtifact: native.BuildCompilerArtifact, BuildRecordSHA256: strings.Repeat("a", 64), Application: Artifact{digest(application), int64(len(application))}, TrustStore: Artifact{digest(ca), int64(len(ca))}, Entrypoint: "service"}
	if err := assembleImage(context.Background(), root, record); err != nil {
		t.Fatal(err)
	}
	return root, record
}

func TestImageBuildHasStableContentsAndConfiguration(t *testing.T) {
	ca := imageTestCA(t, true)
	first, record := imageFixture(t, ca)
	second, repeated := imageFixture(t, ca)
	if !reflect.DeepEqual(record, repeated) {
		t.Fatal("image record changed across paths")
	}
	one, _, err := inventory(filepath.Join(first, "oci"), 5, imageLayerLimit)
	if err != nil {
		t.Fatal(err)
	}
	two, _, err := inventory(filepath.Join(second, "oci"), 5, imageLayerLimit)
	if err != nil || one != two {
		t.Fatal("image layout changed across paths", err)
	}
	extracted := filepath.Join(t.TempDir(), "executable")
	if err := verifyImageContents(context.Background(), filepath.Join(first, "oci"), record, extracted); err != nil {
		t.Fatal(err)
	}
	hash, size, err := fileDigest(extracted)
	if err != nil || (Artifact{hash, size}) != record.Application {
		t.Fatal("extracted executable differs", err)
	}
	config := imageConfig(record)
	if config.Config.User != "65532:65532" || config.Config.Entrypoint[0] != "/service" || config.OS != "linux" || config.Architecture != "amd64" || config.Config.Env[0] != "SSL_CERT_FILE=/"+trustStorePath {
		t.Fatal("runtime image policy differs")
	}
}

func TestTrustStoreRejectsKeysAndNonCACertificates(t *testing.T) {
	ca := imageTestCA(t, true)
	for name, data := range map[string][]byte{"empty": nil, "key": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("private")}), "leaf": imageTestCA(t, false), "leading text": append([]byte("ignored\n"), ca...), "trailing text": append(append([]byte{}, ca...), []byte("ignored")...), "too many": bytes.Repeat(ca, 513), "malformed": []byte("-----BEGIN CERTIFICATE-----\nwrong\n-----END CERTIFICATE-----\n")} {
		t.Run(name, func(t *testing.T) {
			if err := validTrustStore(data); err == nil {
				t.Fatal("unsafe CA input accepted")
			}
		})
	}
	if err := validTrustStore(append(ca, ca...)); err != nil {
		t.Fatal(err)
	}
}

func TestImageRecordAndInputBoundaries(t *testing.T) {
	for _, name := range []string{"../escape", "/service", "etc", "a/b", "name:tag", ".hidden", "name\n"} {
		if validEntrypoint(name) {
			t.Fatal("unsafe entrypoint accepted")
		}
	}
	for _, name := range []string{"application", "hypershell-api", "worker"} {
		if !validEntrypoint(name) {
			t.Fatal("entrypoint rejected")
		}
	}
	root := t.TempDir()
	put(t, root, "source", "original")
	if err := os.Symlink(filepath.Join(root, "source"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"link", "source"} {
		if _, err := captureImageInput(context.Background(), filepath.Join(root, source), filepath.Join(root, "copy-"+source), 3, digest([]byte("original"))); err == nil {
			t.Fatal("link or excess size accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := BuildImage(ctx, ImageOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation lost", err)
	}
	if _, err := VerifyImage(ctx, "", "", "", "", ""); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation lost", err)
	}
}

func TestImageRejectsChangedLayoutAndRecord(t *testing.T) {
	ca := imageTestCA(t, true)
	for _, change := range []string{"extra file", "link", "config bytes", "missing blob", "wrong application", "wrong CA", "wrong diff", "dirty packer", "duplicate index"} {
		t.Run(change, func(t *testing.T) {
			root, record := imageFixture(t, ca)
			image := filepath.Join(root, "oci")
			switch change {
			case "extra file":
				put(t, image, "credentials", "not allowed")
			case "link":
				if err := os.Symlink(filepath.Join(root, "application"), filepath.Join(image, "link")); err != nil {
					t.Fatal(err)
				}
			case "config bytes":
				put(t, image, "blobs/sha256/"+record.Config.SHA256, "changed")
			case "missing blob":
				if err := os.Remove(filepath.Join(image, "blobs/sha256", record.Layer.SHA256)); err != nil {
					t.Fatal(err)
				}
			case "wrong application":
				record.Application.SHA256 = strings.Repeat("0", 64)
			case "wrong CA":
				record.TrustStore.SHA256 = strings.Repeat("0", 64)
			case "wrong diff":
				record.LayerDiffSHA256 = strings.Repeat("0", 64)
			case "dirty packer":
				record.PackerCompiler.SourceState = "dirty"
			case "duplicate index":
				data, err := os.ReadFile(filepath.Join(image, "index.json"))
				if err != nil {
					t.Fatal(err)
				}
				put(t, image, "index.json", strings.Replace(string(data), "{", "{\"schemaVersion\": 2,", 1))
			}
			if err := verifyImageContents(context.Background(), image, record, ""); err == nil {
				t.Fatal("changed image accepted")
			}
		})
	}
}

// Rebuild descriptors after changing a layer or configuration. Correct new
// hashes must not let an image bypass the common runtime policy.
func TestImageRejectsRehashedUnsafeContent(t *testing.T) {
	ca := imageTestCA(t, true)
	for _, change := range []string{"root user", "ambient environment", "writable executable", "symbolic link", "extra executable", "private key"} {
		t.Run(change, func(t *testing.T) {
			root, record := imageFixture(t, ca)
			raw, err := os.ReadFile(filepath.Join(root, "layer.tar"))
			if err != nil {
				t.Fatal(err)
			}
			reader := tar.NewReader(bytes.NewReader(raw))
			var out bytes.Buffer
			writer := tar.NewWriter(&out)
			for {
				header, err := reader.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				data, err := io.ReadAll(reader)
				if err != nil {
					t.Fatal(err)
				}
				if header.Name == record.Entrypoint {
					if change == "writable executable" {
						header.Mode = 0777
					}
					if change == "symbolic link" {
						header.Typeflag = tar.TypeSymlink
						header.Linkname = "/etc/passwd"
						header.Size = 0
						data = nil
					}
				}
				if header.Name == trustStorePath && change == "private key" {
					data = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("secret")})
					header.Size = int64(len(data))
					record.TrustStore = Artifact{digest(data), int64(len(data))}
				}
				if err := writer.WriteHeader(header); err != nil {
					t.Fatal(err)
				}
				if _, err := writer.Write(data); err != nil {
					t.Fatal(err)
				}
			}
			if change == "extra executable" {
				if err := writer.WriteHeader(&tar.Header{Name: "shell", Typeflag: tar.TypeReg, Mode: 0555, Format: tar.FormatUSTAR, ModTime: time.Unix(0, 0)}); err != nil {
					t.Fatal(err)
				}
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			put(t, root, "unsafe-layer.tar", out.String())
			layer, err := tarball.LayerFromFile(filepath.Join(root, "unsafe-layer.tar"), tarball.WithMediaType(types.OCILayer))
			if err != nil {
				t.Fatal(err)
			}
			image, err := mutate.AppendLayers(empty.Image, layer)
			if err != nil {
				t.Fatal(err)
			}
			diff, err := layer.DiffID()
			if err != nil {
				t.Fatal(err)
			}
			record.LayerDiffSHA256 = diff.Hex
			config := imageConfig(record)
			if change == "root user" {
				config.Config.User = "0:0"
			}
			if change == "ambient environment" {
				config.Config.Env = append(config.Config.Env, "TOKEN=private")
			}
			image = mutate.ConfigMediaType(mutate.MediaType(image, types.OCIManifestSchema1), types.OCIConfigJSON)
			image, err = mutate.ConfigFile(image, config)
			if err != nil {
				t.Fatal(err)
			}
			manifest, err := image.RawManifest()
			if err != nil {
				t.Fatal(err)
			}
			configBytes, err := image.RawConfigFile()
			if err != nil {
				t.Fatal(err)
			}
			layerHash, err := layer.Digest()
			if err != nil {
				t.Fatal(err)
			}
			layerSize, err := layer.Size()
			if err != nil {
				t.Fatal(err)
			}
			record.Manifest = Artifact{digest(manifest), int64(len(manifest))}
			record.Config = Artifact{digest(configBytes), int64(len(configBytes))}
			record.Layer = Artifact{layerHash.Hex, layerSize}
			store, err := layout.Write(filepath.Join(root, "unsafe"), empty.Index)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.AppendImage(image); err != nil {
				t.Fatal(err)
			}
			if err := verifyImageContents(context.Background(), string(store), record, ""); err == nil {
				data, _ := json.Marshal(config)
				t.Fatal("rehashed unsafe image accepted", string(data))
			}
		})
	}
}
