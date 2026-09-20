package appbuild

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"github.com/jsell-rh/stego/internal/buildidentity"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

const trustStoreLimit = 1 << 20
const imageMetadataLimit = 64 << 10
const imageLayerLimit = maxFileBytes + trustStoreLimit + (64 << 10)
const trustStorePath = "etc/ssl/certs/ca-certificates.crt"

var entryPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)

type ImageOptions struct {
	BuildRecord, Application, BuildRecordSHA256 string
	TrustStore, TrustStoreSHA256                string
	Entrypoint, Output                          string
}

type ImageRecord struct {
	PackerCompiler         buildidentity.Record `json:"packer_compiler"`
	PackerCompilerArtifact Artifact             `json:"packer_compiler_artifact"`
	Format                 int                  `json:"format"`
	BuildRecordSHA256      string               `json:"build_record_sha256"`
	Application            Artifact             `json:"application"`
	TrustStore             Artifact             `json:"trust_store"`
	Entrypoint             string               `json:"entrypoint"`
	Manifest               Artifact             `json:"manifest"`
	Config                 Artifact             `json:"config"`
	Layer                  Artifact             `json:"layer"`
	LayerDiffSHA256        string               `json:"layer_diff_sha256"`
}

func validEntrypoint(name string) bool { return entryPattern.MatchString(name) && name != "etc" }

func validTrustStore(data []byte) error {
	count := 0
	for rest := bytes.TrimSpace(data); len(rest) > 0; {
		if !bytes.HasPrefix(rest, []byte("-----BEGIN CERTIFICATE-----")) {
			return errors.New("trust store must contain only PEM certificates")
		}
		block, next := pem.Decode(rest)
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			return errors.New("invalid trust store certificate")
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil || !certificate.BasicConstraintsValid || !certificate.IsCA {
			return errors.New("trust store contains a non-CA certificate")
		}
		count++
		if count > 512 {
			return errors.New("trust store has too many certificates")
		}
		rest = bytes.TrimSpace(next)
	}
	if count == 0 || len(data) > trustStoreLimit {
		return errors.New("trust store size or certificate count differs")
	}
	return nil
}

// Copy untrusted paths into a new private directory before any image library
// reads them. A successful copy must have the selected hash and bounded size.
func captureImageInput(ctx context.Context, source, target string, limit int64, expected string) (Artifact, error) {
	if !hashPattern.MatchString(expected) {
		return Artifact{}, errors.New("an expected input digest is required")
	}
	input, err := openInput(source)
	if err != nil {
		return Artifact{}, err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > limit {
		return Artifact{}, errors.New("image input must be a bounded regular file")
	}
	output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return Artifact{}, err
	}
	_, copyErr := io.Copy(output, &contextReader{ctx, io.LimitReader(input, limit+1)})
	closeErr := output.Close()
	if copyErr != nil {
		return Artifact{}, copyErr
	}
	if closeErr != nil {
		return Artifact{}, closeErr
	}
	hash, size, err := fileDigest(target)
	if err != nil || hash != expected || size != info.Size() {
		return Artifact{}, errors.New("captured image input differs")
	}
	return Artifact{hash, size}, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader *contextReader) Read(data []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(data)
}

func newImageDirectory(name string) (string, error) {
	if name == "" {
		return "", errors.New("a new private directory is required")
	}
	absolute, err := filepath.Abs(name)
	if err != nil {
		return "", err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return "", err
	}
	absolute = filepath.Join(parent, filepath.Base(absolute))
	if err := os.Mkdir(absolute, 0700); err != nil {
		return "", err
	}
	return absolute, nil
}

func makeImageLayer(ctx context.Context, target, application, trust, entry string) error {
	file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	writer := tar.NewWriter(file)
	fail := func(err error) error { _ = writer.Close(); _ = file.Close(); return err }
	for _, name := range []string{"etc/", "etc/ssl/", "etc/ssl/certs/"} {
		if err := writer.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: 0755, ModTime: time.Unix(0, 0), Format: tar.FormatUSTAR}); err != nil {
			return fail(err)
		}
	}
	for _, item := range []struct {
		name, source string
		mode         int64
	}{{trustStorePath, trust, 0444}, {entry, application, 0555}} {
		input, err := openInput(item.source)
		if err != nil {
			return fail(err)
		}
		info, err := input.Stat()
		if err != nil {
			_ = input.Close()
			return fail(err)
		}
		if err = writer.WriteHeader(&tar.Header{Name: item.name, Typeflag: tar.TypeReg, Mode: item.mode, Size: info.Size(), ModTime: time.Unix(0, 0), Format: tar.FormatUSTAR}); err == nil {
			_, err = io.CopyN(writer, &contextReader{ctx, input}, info.Size())
		}
		closeErr := input.Close()
		if err != nil {
			return fail(err)
		}
		if closeErr != nil {
			return fail(closeErr)
		}
	}
	if err := writer.Close(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func imageConfig(record *ImageRecord) *v1.ConfigFile {
	return &v1.ConfigFile{Architecture: "amd64", OS: "linux", RootFS: v1.RootFS{Type: "layers", DiffIDs: []v1.Hash{{Algorithm: "sha256", Hex: record.LayerDiffSHA256}}}, Config: v1.Config{User: "65532:65532", Entrypoint: []string{"/" + record.Entrypoint}, WorkingDir: "/", Env: []string{"SSL_CERT_FILE=/" + trustStorePath}, Labels: map[string]string{"org.stego.build-record.sha256": record.BuildRecordSHA256}}}
}

func assembleImage(ctx context.Context, root string, record *ImageRecord) error {
	layerPath := filepath.Join(root, "layer.tar")
	if err := makeImageLayer(ctx, layerPath, filepath.Join(root, "application"), filepath.Join(root, "trust.pem"), record.Entrypoint); err != nil {
		return err
	}
	layer, err := tarball.LayerFromFile(layerPath, tarball.WithMediaType(types.OCILayer))
	if err != nil {
		return err
	}
	image, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		return err
	}
	diff, err := layer.DiffID()
	if err != nil {
		return err
	}
	record.LayerDiffSHA256 = diff.Hex
	image = mutate.MediaType(image, types.OCIManifestSchema1)
	image = mutate.ConfigMediaType(image, types.OCIConfigJSON)
	image, err = mutate.ConfigFile(image, imageConfig(record))
	if err != nil {
		return err
	}
	manifest, err := image.RawManifest()
	if err != nil {
		return err
	}
	config, err := image.RawConfigFile()
	if err != nil {
		return err
	}
	layerHash, err := layer.Digest()
	if err != nil {
		return err
	}
	layerSize, err := layer.Size()
	if err != nil {
		return err
	}
	record.Manifest = Artifact{digest(manifest), int64(len(manifest))}
	record.Config = Artifact{digest(config), int64(len(config))}
	record.Layer = Artifact{layerHash.Hex, layerSize}
	if err := validateImageRecord(record); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	store, err := layout.Write(filepath.Join(root, "oci"), empty.Index)
	if err != nil {
		return err
	}
	if err := store.AppendImage(image); err != nil {
		return err
	}
	return ctx.Err()
}

// BuildImage uses an already verified application record. CA selection is an
// explicit input. This command does not authenticate either expected digest.
func BuildImage(ctx context.Context, options ImageOptions) (*ImageRecord, error) {
	if ctx == nil {
		return nil, errors.New("image build requires a context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return nil, errors.New("image checks require Linux amd64")
	}
	if !validEntrypoint(options.Entrypoint) || !hashPattern.MatchString(options.BuildRecordSHA256) || !hashPattern.MatchString(options.TrustStoreSHA256) {
		return nil, errors.New("image entry point and expected input digests are required")
	}
	root, err := newImageDirectory(options.Output)
	if err != nil {
		return nil, err
	}
	buildPath := filepath.Join(root, "build.json")
	if _, err = captureImageInput(ctx, options.BuildRecord, buildPath, 16<<20, options.BuildRecordSHA256); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(buildPath)
	if err != nil {
		return nil, err
	}
	var build Record
	if err = json.Unmarshal(raw, &build); err != nil {
		return nil, errors.New("invalid application record")
	}
	if _, err = captureImageInput(ctx, options.Application, filepath.Join(root, "application"), maxFileBytes, build.Artifact.SHA256); err != nil {
		return nil, err
	}
	if _, err = Verify(buildPath, filepath.Join(root, "application"), options.BuildRecordSHA256); err != nil {
		return nil, err
	}
	trust, err := captureImageInput(ctx, options.TrustStore, filepath.Join(root, "trust.pem"), trustStoreLimit, options.TrustStoreSHA256)
	if err != nil {
		return nil, err
	}
	ca, err := os.ReadFile(filepath.Join(root, "trust.pem"))
	if err != nil {
		return nil, err
	}
	if err = validTrustStore(ca); err != nil {
		return nil, err
	}
	packer, err := os.Executable()
	if err != nil {
		return nil, err
	}
	packerHash, packerSize, err := fileDigest(packer)
	if err != nil {
		return nil, err
	}
	record := &ImageRecord{PackerCompiler: buildidentity.Current(), PackerCompilerArtifact: Artifact{packerHash, packerSize}, Format: 1, BuildRecordSHA256: options.BuildRecordSHA256, Application: build.Artifact, TrustStore: trust, Entrypoint: options.Entrypoint}
	if err = assembleImage(ctx, root, record); err != nil {
		return nil, err
	}
	if err = verifyImageContents(ctx, filepath.Join(root, "oci"), record, ""); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if err = os.WriteFile(filepath.Join(root, "image.json"), append(data, '\n'), 0600); err != nil {
		return nil, err
	}
	return record, nil
}
