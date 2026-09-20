package appbuild

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

type RegistryImageOptions struct {
	Access                                         RegistryAccess
	Record, RecordSHA256, BuildRecord, Image, Work string
}

type RegistryImageRecord struct {
	Format                int      `json:"format"`
	Operation             string   `json:"operation"`
	Repository            string   `json:"repository"`
	Reference             string   `json:"reference"`
	ImageRecordSHA256     string   `json:"image_record_sha256"`
	RegistryCASHA256      string   `json:"registry_ca_sha256"`
	TokenOrigins          []string `json:"token_origins"`
	BlobOrigins           []string `json:"blob_origins"`
	ImageContentsVerified bool     `json:"image_contents_verified"`
}

func makeRegistryLayout(root string, record *ImageRecord) error {
	if err := os.Mkdir(root, 0700); err != nil {
		return err
	}
	if err := os.Mkdir(filepath.Join(root, "blobs"), 0700); err != nil {
		return err
	}
	if err := os.Mkdir(filepath.Join(root, "blobs", "sha256"), 0700); err != nil {
		return err
	}
	descriptor := imageDescriptor(record.Manifest, types.OCIManifestSchema1)
	descriptor.ArtifactType = string(types.OCIConfigJSON)
	index := v1.IndexManifest{SchemaVersion: 2, MediaType: types.OCIImageIndex, Manifests: []v1.Descriptor{descriptor}}
	data, err := json.MarshalIndent(index, "", "   ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(root, "index.json"), data, 0600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, "oci-layout"), []byte("{\n    \"imageLayoutVersion\": \"1.0.0\"\n}"), 0600)
}

func captureRegistryLayout(ctx context.Context, source, target string, record *ImageRecord) error {
	// These private copies are the only image files read by the network writer.
	if err := makeRegistryLayout(target, record); err != nil {
		return err
	}
	for _, artifact := range []Artifact{record.Manifest, record.Config, record.Layer} {
		relative := filepath.Join("blobs", "sha256", artifact.SHA256)
		actual, err := captureImageInput(ctx, filepath.Join(source, relative), filepath.Join(target, relative), artifact.Size, artifact.SHA256)
		if err != nil {
			return err
		}
		if actual != artifact {
			return errors.New("registry input size differs")
		}
	}
	return verifyImageContents(ctx, target, record, "")
}

func (c *registryConnection) fetch(ctx context.Context, path, target string, expected Artifact) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+c.repository.RegistryStr()+path, nil)
	if err != nil {
		return errors.New("invalid registry retrieval request")
	}
	request.Header.Set("Accept", string(types.OCIManifestSchema1))
	response, err := c.client.Do(request)
	if err != nil {
		return errors.New("registry retrieval failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errors.New("registry retrieval status differs")
	}
	file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	count, copyErr := io.Copy(file, &contextReader{ctx, io.LimitReader(response.Body, expected.Size+1)})
	closeErr := file.Close()
	if copyErr != nil {
		return errors.New("registry response body failed")
	}
	if closeErr != nil {
		return closeErr
	}
	hash, size, err := fileDigest(target)
	if err != nil || count != expected.Size || size != expected.Size || hash != expected.SHA256 {
		return errors.New("retrieved registry bytes differ from the authenticated image record")
	}
	return ctx.Err()
}

func (c *registryConnection) retrieve(ctx context.Context, root string, record *ImageRecord) error {
	if err := makeRegistryLayout(root, record); err != nil {
		return err
	}
	prefix := "/v2/" + c.repository.RepositoryStr() + "/"
	for _, item := range []struct {
		path     string
		artifact Artifact
	}{
		{prefix + "manifests/sha256:" + record.Manifest.SHA256, record.Manifest},
		{prefix + "blobs/sha256:" + record.Config.SHA256, record.Config},
		{prefix + "blobs/sha256:" + record.Layer.SHA256, record.Layer},
	} {
		if err := c.fetch(ctx, item.path, filepath.Join(root, "blobs", "sha256", item.artifact.SHA256), item.artifact); err != nil {
			return err
		}
	}
	return verifyImageContents(ctx, root, record, "")
}

func registryImage(ctx context.Context, options RegistryImageOptions, publish bool) (*RegistryImageRecord, error) {
	if ctx == nil {
		return nil, errors.New("registry work requires a context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return nil, errors.New("registry image checks require Linux amd64")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	record, err := readTrustedImageRecord(options.Record, options.RecordSHA256)
	if err != nil {
		return nil, err
	}
	root, err := newImageDirectory(options.Work)
	if err != nil {
		return nil, err
	}
	savedRecord := filepath.Join(root, "image.json")
	if _, err := captureImageInput(ctx, options.Record, savedRecord, imageMetadataLimit, options.RecordSHA256); err != nil {
		return nil, err
	}
	savedBuild := filepath.Join(root, "build.json")
	if _, err := captureImageInput(ctx, options.BuildRecord, savedBuild, 16<<20, record.BuildRecordSHA256); err != nil {
		return nil, err
	}
	captured := filepath.Join(root, "source-oci")
	if publish {
		if _, err := VerifyImage(ctx, savedRecord, options.Image, savedBuild, options.RecordSHA256, filepath.Join(root, "source-check")); err != nil {
			return nil, err
		}
		if err := captureRegistryLayout(ctx, options.Image, captured, record); err != nil {
			return nil, err
		}
	}
	connection, err := connectRegistry(ctx, options.Access, record, publish)
	if err != nil {
		return nil, err
	}
	defer connection.close()
	if publish {
		store, err := layout.FromPath(captured)
		if err != nil {
			return nil, err
		}
		image, err := store.Image(v1.Hash{Algorithm: "sha256", Hex: record.Manifest.SHA256})
		if err != nil {
			return nil, err
		}
		tag := connection.repository.Tag("sha256-" + record.Manifest.SHA256)
		if err := remote.Write(tag, image, remote.WithContext(ctx), remote.WithTransport(connection.transport), remote.WithJobs(1), remote.WithRetryBackoff(remote.Backoff{Steps: 1}), remote.WithRetryPredicate(func(error) bool { return false })); err != nil {
			return nil, errors.New("registry publication failed")
		}
		if err := connection.fetch(ctx, "/v2/"+connection.repository.RepositoryStr()+"/manifests/"+tag.TagStr(), filepath.Join(root, "published-tag.json"), record.Manifest); err != nil {
			return nil, err
		}
	}
	downloaded := filepath.Join(root, "oci")
	if err := connection.retrieve(ctx, downloaded, record); err != nil {
		return nil, err
	}
	if _, err := VerifyImage(ctx, savedRecord, downloaded, savedBuild, options.RecordSHA256, filepath.Join(root, "retrieved-check")); err != nil {
		return nil, err
	}
	operation := "retrieve"
	if publish {
		operation = "publish"
	}
	result := &RegistryImageRecord{Format: 1, Operation: operation, Repository: options.Access.Repository, Reference: options.Access.Repository + "@sha256:" + record.Manifest.SHA256, ImageRecordSHA256: options.RecordSHA256, RegistryCASHA256: options.Access.CASHA256, TokenOrigins: append([]string{}, options.Access.TokenOrigins...), BlobOrigins: append([]string{}, options.Access.BlobOrigins...), ImageContentsVerified: true}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(root, "registry.json"), append(data, '\n'), 0600); err != nil {
		return nil, err
	}
	return result, nil
}

// PublishImage checks the complete input image, publishes only private captured
// bytes, retrieves them by digest, and checks the complete image again.
func PublishImage(ctx context.Context, options RegistryImageOptions) (*RegistryImageRecord, error) {
	return registryImage(ctx, options, true)
}
func RetrieveImage(ctx context.Context, options RegistryImageOptions) (*RegistryImageRecord, error) {
	return registryImage(ctx, options, false)
}
