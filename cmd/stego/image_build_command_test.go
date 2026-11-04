package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// These tests run stego image build (ST-BLD-CLI04) and stego image sign
// (ST-BLD-CLI05) as an operator does, with the stego binary of the shared
// fixture, on its one real build.

// imageBuildArgs gives the flags of stego image build for the real build of
// the fixture, the CA bundle and the output folder.
func imageBuildArgs(t *testing.T, ca, caSHA256, entrypoint, output string) []string {
	t.Helper()
	app := builtApplication(t)
	return []string{"image", "build", "--build-record", filepath.Join(app.result, "build.json"), "--build-record-sha256", app.recordSHA256, "--application", filepath.Join(app.result, "application"), "--trust-store", ca, "--trust-store-sha256", caSHA256, "--entrypoint", entrypoint, "--output", output}
}

// setFlag gives a copy of the flags with the value of one flag replaced.
func setFlag(flags []string, name, value string) []string {
	changed := append([]string{}, flags...)
	for index := 0; index+1 < len(changed); index++ {
		if changed[index] == "--"+name {
			changed[index+1] = value
		}
	}
	return changed
}

// ociImage is what an image folder holds: its manifest, its configuration
// and the entries of its one layer.
type ociImage struct {
	manifest struct {
		Config struct{ Digest string } `json:"config"`
		Layers []struct{ Digest string } `json:"layers"`
	}
	config struct {
		Config struct {
			User       string   `json:"User"`
			Env        []string `json:"Env"`
			Entrypoint []string `json:"Entrypoint"`
		} `json:"config"`
		RootFS struct {
			DiffIDs []string `json:"diff_ids"`
		} `json:"rootfs"`
	}
	entries map[string]*tar.Header
	files   map[string][]byte
}

// readOCIImage reads the manifest that the image record names, its
// configuration and its layer.
func readOCIImage(t *testing.T, folder, manifest string) ociImage {
	t.Helper()
	blob := func(digest string) []byte {
		data, err := os.ReadFile(filepath.Join(folder, "oci", "blobs", "sha256", strings.TrimPrefix(digest, "sha256:")))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	var image ociImage
	if err := json.Unmarshal(blob(manifest), &image.manifest); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(blob(image.manifest.Config.Digest), &image.config); err != nil {
		t.Fatal(err)
	}
	image.entries, image.files = map[string]*tar.Header{}, map[string][]byte{}
	for _, layer := range image.manifest.Layers {
		var stream io.Reader = bytes.NewReader(blob(layer.Digest))
		if data := blob(layer.Digest); len(data) > 2 && data[0] == 0x1f && data[1] == 0x8b {
			unzipped, err := gzip.NewReader(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			stream = unzipped
		}
		reader := tar.NewReader(stream)
		for {
			header, err := reader.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			image.entries[header.Name] = header
			if header.Typeflag == tar.TypeReg {
				data, err := io.ReadAll(reader)
				if err != nil {
					t.Fatal(err)
				}
				image.files[header.Name] = data
			}
		}
	}
	return image
}

// imageRecordOf reads the manifest digest of an image folder's record.
func imageRecordOf(t *testing.T, folder string) (string, string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(folder, "image.json"))
	if err != nil {
		t.Fatal(err)
	}
	var record struct {
		Manifest   struct{ SHA256 string } `json:"manifest"`
		TrustStore struct{ SHA256 string } `json:"trust_store"`
	}
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	return record.Manifest.SHA256, record.TrustStore.SHA256
}

func TestImageBuildThroughTheCommand(t *testing.T) {
	t.Run("ST-BLD-UC05-R01-T01 stego image build refuses an executable or a CA bundle that is not trusted, and never reads the host's trust store", func(t *testing.T) {
		app := builtApplication(t)
		ca, caSHA256 := imageCA(t)
		hostCA := writeFile(t, t.TempDir(), "host.pem", "not the explicit CA bundle\n", 0600)
		t.Setenv("SSL_CERT_FILE", hostCA)
		t.Setenv("SSL_CERT_DIR", filepath.Dir(hostCA))
		output := filepath.Join(t.TempDir(), "image")
		mustStego(t, imageBuildArgs(t, ca, caSHA256, "service", output)...)
		manifest, trust := imageRecordOf(t, output)
		caBytes, err := os.ReadFile(ca)
		if err != nil {
			t.Fatal(err)
		}
		image := readOCIImage(t, output, manifest)
		if trust != caSHA256 || !bytes.Equal(image.files["etc/ssl/certs/ca-certificates.crt"], caBytes) {
			t.Fatalf("the image's trust store is not the explicit CA bundle: record digest %s, expected %s", trust, caSHA256)
		}
		changed := filepath.Join(t.TempDir(), "application")
		data, err := os.ReadFile(filepath.Join(app.result, "application"))
		if err != nil {
			t.Fatal(err)
		}
		data[len(data)/2] ^= 0xff
		if err := os.WriteFile(changed, data, 0700); err != nil {
			t.Fatal(err)
		}
		zero := strings.Repeat("0", 64)
		for _, item := range []struct {
			name, flag, value, reason string
		}{
			{"an executable that the build record does not name", "application", changed, "captured image input differs"},
			{"a CA bundle whose digest is not trusted", "trust-store-sha256", zero, "captured image input differs"},
			{"the host's trust store as the CA bundle", "trust-store", hostCA, "captured image input differs"},
			{"a build record whose digest is not trusted", "build-record-sha256", zero, "captured image input differs"},
		} {
			output := filepath.Join(t.TempDir(), "image")
			_, stderr, err := runStego(t, setFlag(imageBuildArgs(t, ca, caSHA256, "service", output), item.flag, item.value)...)
			if err == nil || !strings.Contains(stderr, item.reason) {
				t.Errorf("stego image build with %s: %v, %q; expected a refusal that names %q", item.name, err, stderr, item.reason)
			}
			if _, err := os.Stat(filepath.Join(output, "image.json")); !os.IsNotExist(err) {
				t.Errorf("stego image build with %s wrote an image record", item.name)
			}
		}
	})
	t.Run("ST-BLD-UC05-R02-T01 Two runs of stego image build from the same inputs give the same image, which runs as a non-root user", func(t *testing.T) {
		fixture := builtImage(t)
		ca, caSHA256 := imageCA(t)
		output := filepath.Join(t.TempDir(), "image")
		mustStego(t, imageBuildArgs(t, ca, caSHA256, "service", output)...)
		for _, name := range []string{"image.json", "sbom.spdx.json", "sbom.cdx.json"} {
			if fileSHA256(t, filepath.Join(output, name)) != fileSHA256(t, filepath.Join(fixture.folder, name)) {
				t.Errorf("two image builds from the same inputs gave a different %s", name)
			}
		}
		manifest, _ := imageRecordOf(t, output)
		if manifest != fixture.manifest {
			t.Fatalf("two image builds from the same inputs gave the images sha256:%s and sha256:%s", fixture.manifest, manifest)
		}
		image := readOCIImage(t, output, manifest)
		if image.config.Config.User != "65532:65532" {
			t.Errorf("the image runs as %q; expected the non-root user 65532:65532", image.config.Config.User)
		}
	})
	t.Run("ST-BLD-UC05-R02-T02 The image that stego image build writes is on scratch: one layer with only the executable and the CA bundle", func(t *testing.T) {
		fixture := builtImage(t)
		image := readOCIImage(t, fixture.folder, fixture.manifest)
		if len(image.manifest.Layers) != 1 || len(image.config.RootFS.DiffIDs) != 1 {
			t.Fatalf("the image has %d layers and %d layer IDs; an image on scratch has exactly its own one", len(image.manifest.Layers), len(image.config.RootFS.DiffIDs))
		}
		var names []string
		for name := range image.entries {
			names = append(names, name)
		}
		sort.Strings(names)
		if strings.Join(names, " ") != "etc/ etc/ssl/ etc/ssl/certs/ etc/ssl/certs/ca-certificates.crt service" {
			t.Fatalf("the layer holds %v; expected only the folders of the CA bundle, the CA bundle and the executable", names)
		}
		if mode := image.entries["service"].Mode; mode != 0555 {
			t.Errorf("the executable has mode %o; expected 0555, which no user can write", mode)
		}
		if mode := image.entries["etc/ssl/certs/ca-certificates.crt"].Mode; mode != 0444 {
			t.Errorf("the CA bundle has mode %o; expected 0444", mode)
		}
		if strings.Join(image.config.Config.Env, " ") != "SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt" || strings.Join(image.config.Config.Entrypoint, " ") != "/service" {
			t.Errorf("the image has the environment %v and the entry point %v; expected only the CA bundle path and /service", image.config.Config.Env, image.config.Config.Entrypoint)
		}
	})
	t.Run("ST-BLD-UC05-R03-T01 stego image sign accepts only the signature by the trusted signer that the operator names", func(t *testing.T) {
		fixture := builtImage(t)
		gh := fakeGH(t)
		sign := func(subject string, signature func(gh, spdx, cyclonedx string) []string) (string, string, error) {
			image := copyFolder(t, fixture.folder)
			spdx, cyclonedx := signatureBundles(t, image, subject)
			_, stderr, err := runStego(t, append([]string{"image", "sign", "--image", image, "--record-sha256", fixture.recordSHA256}, signature(gh, spdx, cyclonedx)...)...)
			return image, stderr, err
		}
		image, stderr, err := sign(trustedSubject, signatureArgs)
		if err != nil {
			t.Fatalf("stego image sign refused the trusted signer: %v\n%s", err, stderr)
		}
		if _, err := os.Stat(filepath.Join(image, "signature.json")); err != nil {
			t.Fatalf("stego image sign wrote no signature record: %v", err)
		}
		for _, item := range []struct {
			name      string
			subject   string
			signature func(gh, spdx, cyclonedx string) []string
		}{
			{"bundles by another workflow", otherSubject, signatureArgs},
			{"another trusted subject than the bundles' signer", trustedSubject, func(gh, spdx, cyclonedx string) []string {
				return setFlag(signatureArgs(gh, spdx, cyclonedx), "signer-subject", otherSubject)
			}},
			{"another trusted issuer than the bundles' issuer", trustedSubject, func(gh, spdx, cyclonedx string) []string {
				return setFlag(signatureArgs(gh, spdx, cyclonedx), "signer-issuer", "https://issuer.example.test")
			}},
		} {
			image, stderr, err := sign(item.subject, item.signature)
			if err == nil || !strings.Contains(stderr, "not by the trusted signer") {
				t.Errorf("stego image sign with %s: %v, %q; expected a refusal that names the trusted signer", item.name, err, stderr)
			}
			if _, err := os.Stat(filepath.Join(image, "signature.json")); !os.IsNotExist(err) {
				t.Errorf("stego image sign with %s wrote a signature record", item.name)
			}
		}
	})
	t.Run("ST-BLD-UC05-R03-T02 stego image sign refuses a signature of another image and a signature that does not carry both SBOMs", func(t *testing.T) {
		fixture := builtImage(t)
		ca, caSHA256 := imageCA(t)
		other := filepath.Join(t.TempDir(), "other")
		mustStego(t, imageBuildArgs(t, ca, caSHA256, "other", other)...)
		otherSpdx, otherCyclonedx := signatureBundles(t, other, trustedSubject)
		gh := fakeGH(t)
		for _, item := range []struct {
			name, reason string
			bundles      func(image string) (string, string)
		}{
			{"the signature of another image", "not exactly the image", func(string) (string, string) { return otherSpdx, otherCyclonedx }},
			{"the SPDX attestation as both", "CycloneDX", func(image string) (string, string) {
				spdx, _ := signatureBundles(t, image, trustedSubject)
				return spdx, spdx
			}},
		} {
			image := copyFolder(t, fixture.folder)
			spdx, cyclonedx := item.bundles(image)
			_, stderr, err := runStego(t, append([]string{"image", "sign", "--image", image, "--record-sha256", fixture.recordSHA256}, signatureArgs(gh, spdx, cyclonedx)...)...)
			if err == nil || !strings.Contains(stderr, item.reason) {
				t.Errorf("stego image sign with %s: %v, %q; expected a refusal that names %q", item.name, err, stderr, item.reason)
			}
			if _, err := os.Stat(filepath.Join(image, "signature.json")); !os.IsNotExist(err) {
				t.Errorf("stego image sign with %s wrote a signature record", item.name)
			}
		}
	})
}
