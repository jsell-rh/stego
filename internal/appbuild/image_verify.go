package appbuild

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

func validateImageRecord(record *ImageRecord) error {
	if record.PackerCompiler.Validate() != nil || record.PackerCompiler.SourceState != "clean" || record.Format != 1 || !hashPattern.MatchString(record.BuildRecordSHA256) || !hashPattern.MatchString(record.LayerDiffSHA256) || !validEntrypoint(record.Entrypoint) {
		return errors.New("invalid image record policy")
	}
	for _, input := range []struct {
		artifact Artifact
		limit    int64
	}{{record.PackerCompilerArtifact, maxFileBytes}, {record.Application, maxFileBytes}, {record.TrustStore, trustStoreLimit}, {record.Manifest, imageMetadataLimit}, {record.Config, imageMetadataLimit}, {record.Layer, imageLayerLimit}} {
		if !hashPattern.MatchString(input.artifact.SHA256) || input.artifact.Size < 1 || input.artifact.Size > input.limit {
			return errors.New("invalid image artifact identity")
		}
	}
	return nil
}

func imageDescriptor(artifact Artifact, media types.MediaType) v1.Descriptor {
	return v1.Descriptor{MediaType: media, Size: artifact.Size, Digest: v1.Hash{Algorithm: "sha256", Hex: artifact.SHA256}}
}

func checkImageBlob(ctx context.Context, name string, expected Artifact) error {
	file, err := openInput(name)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != expected.Size {
		return errors.New("image blob size differs")
	}
	hash := sha256.New()
	count, err := io.Copy(hash, &contextReader{ctx, io.LimitReader(file, expected.Size+1)})
	if err != nil {
		return err
	}
	if count != expected.Size || hex.EncodeToString(hash.Sum(nil)) != expected.SHA256 {
		return errors.New("image blob content differs")
	}
	return nil
}

func readImageBytes(name string, limit int64) ([]byte, error) {
	file, err := openInput(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > limit {
		return nil, errors.New("invalid image metadata size")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) != info.Size() {
		return nil, errors.New("image metadata changed size")
	}
	return data, nil
}

// This is the static Go image profile, not a general OCI image policy. An
// image from another producer must satisfy the same complete content policy.
func verifyImageContents(ctx context.Context, root string, record *ImageRecord, applicationOutput string) error {
	if err := validateImageRecord(record); err != nil {
		return err
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() {
		return errors.New("image layout must be a real directory")
	}
	expected := map[string]Artifact{}
	for _, artifact := range []Artifact{record.Manifest, record.Config, record.Layer} {
		expected["blobs/sha256/"+artifact.SHA256] = artifact
	}
	if len(expected) != 3 {
		return errors.New("image blob roles overlap")
	}
	seen := map[string]bool{}
	err = filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if name == root {
			return nil
		}
		relative, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if entry.IsDir() {
			if relative != "blobs" && relative != "blobs/sha256" {
				return errors.New("unexpected image directory")
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return errors.New("image layout contains a link or special file")
		}
		if seen[relative] || len(seen) >= 5 {
			return errors.New("image layout file count differs")
		}
		seen[relative] = true
		if artifact, ok := expected[relative]; ok {
			return checkImageBlob(ctx, name, artifact)
		}
		if relative != "index.json" && relative != "oci-layout" {
			return errors.New("unexpected image layout file")
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(seen) != 5 {
		return errors.New("image layout is incomplete")
	}
	marker, err := readImageBytes(filepath.Join(root, "oci-layout"), 1024)
	if err != nil || string(marker) != "{\n    \"imageLayoutVersion\": \"1.0.0\"\n}" {
		return errors.New("image layout version differs")
	}
	index := v1.IndexManifest{SchemaVersion: 2, MediaType: types.OCIImageIndex, Manifests: []v1.Descriptor{imageDescriptor(record.Manifest, types.OCIManifestSchema1)}}
	expectedIndex, _ := json.MarshalIndent(index, "", "   ")
	actualIndex, err := readImageBytes(filepath.Join(root, "index.json"), imageMetadataLimit)
	if err != nil || !bytes.Equal(actualIndex, expectedIndex) {
		return errors.New("image index differs from the selected manifest")
	}
	manifest := v1.Manifest{SchemaVersion: 2, MediaType: types.OCIManifestSchema1, Config: imageDescriptor(record.Config, types.OCIConfigJSON), Layers: []v1.Descriptor{imageDescriptor(record.Layer, types.OCILayer)}}
	expectedManifest, _ := json.Marshal(manifest)
	actualManifest, err := readImageBytes(filepath.Join(root, "blobs/sha256", record.Manifest.SHA256), imageMetadataLimit)
	if err != nil || !bytes.Equal(actualManifest, expectedManifest) {
		return errors.New("image manifest differs from the required profile")
	}
	expectedConfig, _ := json.Marshal(imageConfig(record))
	actualConfig, err := readImageBytes(filepath.Join(root, "blobs/sha256", record.Config.SHA256), imageMetadataLimit)
	if err != nil || !bytes.Equal(actualConfig, expectedConfig) {
		return errors.New("image configuration differs from the required profile")
	}
	store, err := layout.FromPath(root)
	if err != nil {
		return err
	}
	image, err := store.Image(v1.Hash{Algorithm: "sha256", Hex: record.Manifest.SHA256})
	if err != nil {
		return err
	}
	layers, err := image.Layers()
	if err != nil || len(layers) != 1 {
		return errors.New("image layer count differs")
	}
	stream, err := layers[0].Uncompressed()
	if err != nil {
		return err
	}
	defer stream.Close()
	diff := sha256.New()
	limited := &io.LimitedReader{R: io.TeeReader(&contextReader{ctx, stream}, diff), N: imageLayerLimit + 1}
	reader := tar.NewReader(limited)
	seenEntries := map[string]bool{}
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if seenEntries[header.Name] || len(seenEntries) >= 5 {
			return errors.New("image has repeated or extra entries")
		}
		seenEntries[header.Name] = true
		if header.Format != tar.FormatUSTAR || header.Uid != 0 || header.Gid != 0 || header.Uname != "" || header.Gname != "" || header.Linkname != "" || header.Devmajor != 0 || header.Devminor != 0 || !header.ModTime.Equal(time.Unix(0, 0)) || len(header.PAXRecords) > 0 || len(header.Xattrs) > 0 {
			return errors.New("image entry metadata differs")
		}
		switch header.Name {
		case "etc/", "etc/ssl/", "etc/ssl/certs/":
			if header.Typeflag != tar.TypeDir || header.Mode != 0755 || header.Size != 0 {
				return errors.New("image directory metadata differs")
			}
		case trustStorePath:
			if header.Typeflag != tar.TypeReg || header.Mode != 0444 || header.Size != record.TrustStore.Size {
				return errors.New("image trust store metadata differs")
			}
			data, err := io.ReadAll(io.LimitReader(reader, trustStoreLimit+1))
			if err != nil {
				return err
			}
			if digest(data) != record.TrustStore.SHA256 {
				return errors.New("image trust store content differs")
			}
			if err := validTrustStore(data); err != nil {
				return err
			}
		case record.Entrypoint:
			if header.Typeflag != tar.TypeReg || header.Mode != 0555 || header.Size != record.Application.Size {
				return errors.New("image executable metadata differs")
			}
			hash := sha256.New()
			var output *os.File
			var destination io.Writer = hash
			if applicationOutput != "" {
				output, err = os.OpenFile(applicationOutput, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
				if err != nil {
					return err
				}
				destination = io.MultiWriter(hash, output)
			}
			count, copyErr := io.CopyN(destination, reader, header.Size)
			var closeErr error
			if output != nil {
				closeErr = output.Close()
			}
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
			if count != record.Application.Size || hex.EncodeToString(hash.Sum(nil)) != record.Application.SHA256 {
				return errors.New("image executable content differs")
			}
		default:
			return errors.New("unexpected image filesystem entry")
		}
	}
	trailing, err := io.ReadAll(io.LimitReader(limited, 1))
	if err != nil {
		return err
	}
	if len(trailing) != 0 || limited.N <= 0 || len(seenEntries) != 5 || hex.EncodeToString(diff.Sum(nil)) != record.LayerDiffSHA256 {
		return errors.New("image layer content or size differs")
	}
	return ctx.Err()
}

func VerifyImage(ctx context.Context, recordPath, imageRoot, buildPath, expectedSHA256, work string) (*ImageRecord, error) {
	if ctx == nil {
		return nil, errors.New("image verification requires a context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := readImageBytes(recordPath, imageMetadataLimit)
	if err != nil {
		return nil, err
	}
	if !hashPattern.MatchString(expectedSHA256) || digest(data) != expectedSHA256 {
		return nil, errors.New("image record digest differs")
	}
	var record ImageRecord
	if err = json.Unmarshal(data, &record); err != nil {
		return nil, errors.New("invalid image record")
	}
	canonical, err := json.MarshalIndent(record, "", "  ")
	if err != nil || !bytes.Equal(data, append(canonical, '\n')) {
		return nil, errors.New("image record is not canonical")
	}
	if err = validateImageRecord(&record); err != nil {
		return nil, err
	}
	root, err := newImageDirectory(work)
	if err != nil {
		return nil, err
	}
	savedBuild := filepath.Join(root, "build.json")
	if _, err = captureImageInput(ctx, buildPath, savedBuild, 16<<20, record.BuildRecordSHA256); err != nil {
		return nil, err
	}
	application := filepath.Join(root, "application")
	if err = verifyImageContents(ctx, imageRoot, &record, application); err != nil {
		return nil, err
	}
	build, err := Verify(savedBuild, application, record.BuildRecordSHA256)
	if err != nil {
		return nil, err
	}
	if build.Artifact != record.Application {
		return nil, errors.New("image differs from the verified application")
	}
	return &record, ctx.Err()
}
