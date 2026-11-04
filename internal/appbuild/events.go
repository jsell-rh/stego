package appbuild

import (
	"fmt"
	"path/filepath"
)

// The event types of Building (ST-BLD-EV01 to ST-BLD-EV06). Each is delivered
// in process, and nothing keeps it between runs. A digest is the lower-case
// hexadecimal SHA-256 of the bytes it names.

// ModulesResolved tells that the module requirements hold the service's
// resolved modules (ST-BLD-EV01).
type ModulesResolved struct {
	// ModuleFileDigest is the digest of the new go.mod.
	ModuleFileDigest string `json:"module_file_digest"`
	// SumFileDigest is the digest of the new go.sum.
	SumFileDigest string `json:"sum_file_digest"`
}

// ModuleCacheDownloaded tells that a module cache for one source revision is
// ready for offline builds (ST-BLD-EV02).
type ModuleCacheDownloaded struct {
	// SourceRevision is the commit the cache serves.
	SourceRevision string `json:"source_revision"`
	// InventoryDigest is the digest of the cache record.
	InventoryDigest string `json:"inventory_digest"`
}

// ApplicationBuilt tells that a service's executable was built and recorded,
// so it can be imaged and checked (ST-BLD-EV03).
type ApplicationBuilt struct {
	// SourceRevision is the commit built.
	SourceRevision string `json:"source_revision"`
	// RecordDigest is the digest of the build record.
	RecordDigest string `json:"record_digest"`
	// ExecutableDigest is the digest of the executable.
	ExecutableDigest string `json:"executable_digest"`
}

// ImageBuilt tells that an image of a built executable exists locally with
// its record (ST-BLD-EV04).
type ImageBuilt struct {
	// ImageDigest is the digest of the image.
	ImageDigest string `json:"image_digest"`
	// BuildRecordDigest is the digest of the build record the image came from.
	BuildRecordDigest string `json:"build_record_digest"`
}

// ImageSigned tells that an image and its SBOMs carry a keyless signature
// (ST-BLD-EV05).
type ImageSigned struct {
	// ImageDigest is the digest of the signed image.
	ImageDigest string `json:"image_digest"`
	// Signer is the identity that signed.
	Signer string `json:"signer"`
}

// ImagePublished tells that a signed image is in a registry, ready to deploy
// (ST-BLD-EV06).
type ImagePublished struct {
	// ImageDigest is the digest of the published image.
	ImageDigest string `json:"image_digest"`
	// Repository is where the image was published, such as
	// quay.io/example/service.
	Repository string `json:"repository"`
}

// ApplicationBuiltIn gives the Application built event of a build whose
// result folder is output: it reads the build record there, checks it as a
// build record at the digest of the bytes it read, and names the revision,
// the record digest and the executable digest.
func ApplicationBuiltIn(output string) (ApplicationBuilt, error) {
	name := filepath.Join(output, "build.json")
	data, err := readImageBytes(name, 16<<20)
	if err != nil {
		return ApplicationBuilt{}, fmt.Errorf("cannot read the build record %s: %w", name, err)
	}
	record, err := readBuildRecord(name, digest(data))
	if err != nil {
		return ApplicationBuilt{}, err
	}
	return ApplicationBuilt{SourceRevision: record.SourceRevision, RecordDigest: digest(data), ExecutableDigest: record.Artifact.SHA256}, nil
}
