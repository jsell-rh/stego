package appbuild

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
)

// The policies of Building (ST-BLD-PO01 to ST-BLD-PO03). Each event handler
// hears one event in process and sends the next command at its next run. It
// takes what the command reads from the event and from the folder that the
// command which raised the event wrote, never from the caller, and it gives
// the event that the command it sends raises.

// BuildImageOnApplicationBuilt is the event handler of ST-BLD-PO01. It hears
// Application built and sends Build the image: it reads the build record in
// the result folder at the digest the event names, refuses a record whose
// revision or executable is not the one the event names, and builds the image
// from the executable the record names only. The build record, its digest and
// the executable come from the event and the result folder; the caller gives
// only the CA bundle, its digest, the entry point and the output folder. It
// gives Image built.
func BuildImageOnApplicationBuilt(ctx context.Context, event ApplicationBuilt, result string, options ImageOptions) (*ImageRecord, ImageBuilt, error) {
	if result == "" {
		return nil, ImageBuilt{}, errors.New("the image build of Application built needs the build result folder")
	}
	if options.BuildRecord != "" || options.BuildRecordSHA256 != "" || options.Application != "" {
		return nil, ImageBuilt{}, errors.New("the image build of Application built takes the build record and the executable from the event only")
	}
	recordPath := filepath.Join(result, "build.json")
	build, err := readBuildRecord(recordPath, event.RecordDigest)
	if err != nil {
		return nil, ImageBuilt{}, err
	}
	if build.SourceRevision != event.SourceRevision {
		return nil, ImageBuilt{}, fmt.Errorf("the build record is of revision %s, not of revision %s that Application built names", build.SourceRevision, event.SourceRevision)
	}
	if build.Artifact.SHA256 != event.ExecutableDigest {
		return nil, ImageBuilt{}, fmt.Errorf("the build record names the executable sha256:%s, not sha256:%s that Application built names", build.Artifact.SHA256, event.ExecutableDigest)
	}
	options.BuildRecord = recordPath
	options.BuildRecordSHA256 = event.RecordDigest
	options.Application = filepath.Join(result, "application")
	record, err := BuildImage(ctx, options)
	if err != nil {
		return nil, ImageBuilt{}, err
	}
	return record, ImageBuilt{ImageDigest: record.Manifest.SHA256, BuildRecordDigest: record.BuildRecordSHA256}, nil
}
