package appbuild

import (
	"bytes"
	"context"
	"encoding/json"
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

// SignImageOnImageBuilt is the event handler of ST-BLD-PO02. It hears Image
// built and sends Sign the image: it reads the image record in the image
// folder, refuses a record whose image or build record is not the one the
// event names, and checks the provider's signature of that image digest with
// SignImage, at the digest of the record it read. It gives Image signed.
func SignImageOnImageBuilt(ctx context.Context, event ImageBuilt, image string, check SignatureCheck) (*ImageSignature, ImageSigned, error) {
	if image == "" {
		return nil, ImageSigned{}, errors.New("the signing of Image built needs the image folder")
	}
	data, err := readImageBytes(filepath.Join(image, "image.json"), imageMetadataLimit)
	if err != nil {
		return nil, ImageSigned{}, fmt.Errorf("cannot read the image record of Image built: %w", err)
	}
	recordSHA256 := digest(data)
	record, err := readTrustedImageRecord(filepath.Join(image, "image.json"), recordSHA256)
	if err != nil {
		return nil, ImageSigned{}, err
	}
	if record.Manifest.SHA256 != event.ImageDigest {
		return nil, ImageSigned{}, fmt.Errorf("the image record names the image sha256:%s, not sha256:%s that Image built names", record.Manifest.SHA256, event.ImageDigest)
	}
	if record.BuildRecordSHA256 != event.BuildRecordDigest {
		return nil, ImageSigned{}, fmt.Errorf("the image record names the build record sha256:%s, not sha256:%s that Image built names", record.BuildRecordSHA256, event.BuildRecordDigest)
	}
	signature, err := SignImage(ctx, SignOptions{Image: image, RecordSHA256: recordSHA256, Check: check})
	if err != nil {
		return nil, ImageSigned{}, err
	}
	return signature, SignedEvent(signature), nil
}

// SignedEvent gives the Image signed event of a signature that SignImage
// checked: the image digest and the signer.
func SignedEvent(signature *ImageSignature) ImageSigned {
	return ImageSigned{ImageDigest: signature.ImageSHA256, Signer: signature.Signer.describe()}
}

// PublishImageOnImageSigned is the event handler of ST-BLD-PO03. It hears
// Image signed and sends Publish the image: it reads signature.json in the
// image folder, refuses an image with no signature, or with a signature of
// another image or signer than the event names, checks the signature again
// with the provider's verifier and the trusted signer, and refuses it unless
// it verifies and is the same signature. Only then does it publish the image
// of the record the signature names, with PublishImage. The record, the build
// record and the image come from the image folder; the caller gives the
// registry access, the work folder and the signature check. It gives Image
// published.
func PublishImageOnImageSigned(ctx context.Context, event ImageSigned, image string, options RegistryImageOptions) (*RegistryImageRecord, ImagePublished, error) {
	if ctx == nil {
		return nil, ImagePublished{}, errors.New("the publication of Image signed requires a context")
	}
	if image == "" {
		return nil, ImagePublished{}, errors.New("the publication of Image signed needs the image folder")
	}
	if options.Record != "" || options.RecordSHA256 != "" || options.BuildRecord != "" || options.Image != "" {
		return nil, ImagePublished{}, errors.New("the publication of Image signed takes the image record, the build record and the image from the image folder only")
	}
	if err := options.Signature.validate(); err != nil {
		return nil, ImagePublished{}, err
	}
	data, err := readImageBytes(filepath.Join(image, "signature.json"), imageMetadataLimit)
	if err != nil {
		return nil, ImagePublished{}, fmt.Errorf("the image is not signed, so it is not published: %w", err)
	}
	var signed ImageSignature
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&signed); err != nil {
		return nil, ImagePublished{}, errors.New("the image signature record is not valid, so the image is not published")
	}
	if signed.ImageSHA256 != event.ImageDigest {
		return nil, ImagePublished{}, fmt.Errorf("the signature is about the image sha256:%s, not sha256:%s that Image signed names", signed.ImageSHA256, event.ImageDigest)
	}
	if signed.Signer.describe() != event.Signer {
		return nil, ImagePublished{}, fmt.Errorf("the signature is by %s, not by %s that Image signed names", signed.Signer.describe(), event.Signer)
	}
	recordPath := filepath.Join(image, "image.json")
	record, err := readTrustedImageRecord(recordPath, signed.ImageRecordSHA256)
	if err != nil {
		return nil, ImagePublished{}, err
	}
	if record.Manifest.SHA256 != signed.ImageSHA256 {
		return nil, ImagePublished{}, errors.New("the signed image record names another image than the signature")
	}
	buildPath := filepath.Join(image, "build.json")
	build, err := readBuildRecord(buildPath, record.BuildRecordSHA256)
	if err != nil {
		return nil, ImagePublished{}, err
	}
	manifest := filepath.Join(image, "oci", "blobs", "sha256", record.Manifest.SHA256)
	checked, err := checkImageSignature(ctx, options.Signature, manifest, record, signed.ImageRecordSHA256, build)
	if err != nil {
		return nil, ImagePublished{}, fmt.Errorf("the image signature does not verify, so the image is not published: %w", err)
	}
	if !sameSignature(*checked, signed) {
		return nil, ImagePublished{}, errors.New("the image signature record differs from the signature that verifies, so the image is not published")
	}
	options.Record = recordPath
	options.RecordSHA256 = signed.ImageRecordSHA256
	options.BuildRecord = buildPath
	options.Image = filepath.Join(image, "oci")
	published, err := PublishImage(ctx, options)
	if err != nil {
		return nil, ImagePublished{}, err
	}
	return published, ImagePublished{ImageDigest: record.Manifest.SHA256, Repository: published.Repository}, nil
}

// sameSignature tells whether a signature that verified now is the one the
// signature record holds: the same image record, image, signer, log entry and
// attestations. The signing time is not compared, as a verifier may give it
// at another precision.
func sameSignature(checked, recorded ImageSignature) bool {
	if checked.Format != recorded.Format || checked.ImageRecordSHA256 != recorded.ImageRecordSHA256 || checked.ImageSHA256 != recorded.ImageSHA256 || checked.Signer != recorded.Signer || checked.TransparencyLogEntry != recorded.TransparencyLogEntry || len(checked.Attestations) != len(recorded.Attestations) {
		return false
	}
	for index := range checked.Attestations {
		if checked.Attestations[index] != recorded.Attestations[index] {
			return false
		}
	}
	return true
}
