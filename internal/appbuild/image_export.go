package appbuild

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

const imageArchiveLimit = imageLayerLimit + (1 << 20)

type ImageExportRecord struct {
	Format                  int      `json:"format"`
	ImageRecordSHA256       string   `json:"image_record_sha256"`
	SourceOCIManifestSHA256 string   `json:"source_oci_manifest_sha256"`
	ConfigurationSHA256     string   `json:"configuration_sha256"`
	Archive                 Artifact `json:"archive"`
}

func imageArchiveIdentity(ctx context.Context, path string) (Artifact, error) {
	file, err := openInput(path)
	if err != nil {
		return Artifact{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > imageArchiveLimit {
		return Artifact{}, errors.New("image archive must be a bounded regular file")
	}
	hash := sha256.New()
	size, err := io.Copy(hash, &contextReader{ctx, io.LimitReader(file, imageArchiveLimit+1)})
	if err != nil {
		return Artifact{}, err
	}
	if size != info.Size() {
		return Artifact{}, errors.New("image archive changed size")
	}
	return Artifact{hex.EncodeToString(hash.Sum(nil)), size}, nil
}

// Use the library's transport writer for engines with a Docker archive loader.
// The OCI publication identity stays separate from this transport archive.
func exportImageArchive(ctx context.Context, imageRoot string, record *ImageRecord, output string) (Artifact, error) {
	if err := verifyImageContents(ctx, imageRoot, record, ""); err != nil {
		return Artifact{}, err
	}
	store, err := layout.FromPath(imageRoot)
	if err != nil {
		return Artifact{}, err
	}
	image, err := store.Image(v1.Hash{Algorithm: "sha256", Hex: record.Manifest.SHA256})
	if err != nil {
		return Artifact{}, err
	}
	ref, err := name.NewTag("stego.invalid/application:sha256-"+record.Manifest.SHA256, name.StrictValidation)
	if err != nil {
		return Artifact{}, err
	}
	file, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return Artifact{}, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	writer := &boundedWriter{destination: file, left: imageArchiveLimit, cancel: cancel}
	writeErr := tarball.Write(ref, image, writer)
	closeErr := file.Close()
	if writeErr != nil {
		return Artifact{}, writeErr
	}
	if closeErr != nil {
		return Artifact{}, closeErr
	}
	if err := ctx.Err(); err != nil {
		return Artifact{}, err
	}
	identity, err := imageArchiveIdentity(ctx, output)
	if err != nil {
		return Artifact{}, err
	}
	// Inspect the saved transport, not the in-memory source image.
	saved, err := tarball.ImageFromPath(output, nil)
	if err != nil {
		return Artifact{}, err
	}
	config, err := saved.RawConfigFile()
	if err != nil || digest(config) != record.Config.SHA256 {
		return Artifact{}, errors.New("exported image configuration differs")
	}
	layers, err := saved.Layers()
	if err != nil || len(layers) != 1 {
		return Artifact{}, errors.New("exported image layer count differs")
	}
	layerHash, err := layers[0].Digest()
	if err != nil || layerHash != (v1.Hash{Algorithm: "sha256", Hex: record.Layer.SHA256}) {
		return Artifact{}, errors.New("exported image layer differs")
	}
	diff, err := layers[0].DiffID()
	if err != nil || diff != (v1.Hash{Algorithm: "sha256", Hex: record.LayerDiffSHA256}) {
		return Artifact{}, errors.New("exported image filesystem differs")
	}
	after, err := imageArchiveIdentity(ctx, output)
	if err != nil || after != identity {
		return Artifact{}, errors.New("exported image archive changed")
	}
	return identity, ctx.Err()
}

// ExportImage verifies the complete image and native build record first. It
// writes image.docker.tar and export.json under the new private work directory.
func ExportImage(ctx context.Context, recordPath, imageRoot, buildPath, expectedSHA256, work string) (*ImageExportRecord, error) {
	record, err := VerifyImage(ctx, recordPath, imageRoot, buildPath, expectedSHA256, work)
	if err != nil {
		return nil, err
	}
	artifact, err := exportImageArchive(ctx, imageRoot, record, filepath.Join(work, "image.docker.tar"))
	if err != nil {
		return nil, err
	}
	result := &ImageExportRecord{Format: 1, ImageRecordSHA256: expectedSHA256, SourceOCIManifestSHA256: record.Manifest.SHA256, ConfigurationSHA256: record.Config.SHA256, Archive: artifact}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(work, "export.json"), append(data, '\n'), 0600); err != nil {
		return nil, err
	}
	return result, nil
}
