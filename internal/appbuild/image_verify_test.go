package appbuild

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestImageVerificationChecksBuildAndSigner(t *testing.T) {
	t.Run("ST-BLD-UC04-R04-T01 An image matches only when its record, its trusted build record and its signature by the trusted signer all pass", func(t *testing.T) {
		root, record, recordSHA256, sboms := signFixture(t)
		build, err := readBuildRecord(filepath.Join(root, "build.json"), record.BuildRecordSHA256)
		if err != nil {
			t.Fatal(err)
		}
		manifest := filepath.Join(root, "oci", "blobs", "sha256", record.Manifest.SHA256)
		check := fixtureSignatureCheck(t, record, sboms)
		signature, err := checkImageSignature(context.Background(), check, manifest, record, recordSHA256, build)
		if err != nil {
			t.Fatal(err)
		}
		var reader ImageVerifyReader
		if _, err := reader.Verification(); err == nil {
			t.Fatal("an empty reader gave a verification")
		}
		reader.hold(ImageVerification{Record: record, Build: build})
		if _, err := reader.Verification(); err == nil {
			t.Fatal("a verification without a signature was given")
		}
		reader.hold(ImageVerification{Record: record, Build: build, Signature: signature})
		if held, err := reader.Verification(); err != nil || held.Signature.ImageSHA256 != record.Manifest.SHA256 {
			t.Fatal("the full verification was not given", err)
		}
		other := check
		verifier := fakeVerifier{}
		for index, bundle := range []string{check.SPDXBundle, check.CycloneDXBundle} {
			attestation := signedSBOM(record, sboms[index])
			attestation.Signer.Subject = strings.Replace(attestation.Signer.Subject, "platform", "fork", 1)
			verifier[bundle] = attestation
		}
		other.Verifier = verifier
		if _, err := checkImageSignature(context.Background(), other, manifest, record, recordSHA256, build); err == nil {
			t.Fatal("an image signed by another identity matched")
		}
		unsigned := check
		unsigned.Verifier = fakeVerifier{}
		if _, err := checkImageSignature(context.Background(), unsigned, manifest, record, recordSHA256, build); err == nil {
			t.Fatal("an unsigned image matched")
		}
		changed := *build
		changed.Artifact.SHA256 = strings.Repeat("0", 64)
		if _, err := checkImageSignature(context.Background(), check, manifest, record, recordSHA256, &changed); err == nil {
			t.Fatal("an image whose build record differs matched")
		}
		for name, change := range map[string]func(*ImageVerifyOptions){
			"no record digest":    func(o *ImageVerifyOptions) { o.RecordSHA256 = "" },
			"no SPDX bundle":      func(o *ImageVerifyOptions) { o.Check.SPDXBundle = "" },
			"no CycloneDX bundle": func(o *ImageVerifyOptions) { o.Check.CycloneDXBundle = "" },
			"no trusted signer":   func(o *ImageVerifyOptions) { o.Check.Signer = SignerIdentity{} },
			"no verifier":         func(o *ImageVerifyOptions) { o.Check.Verifier = nil },
		} {
			t.Run(name, func(t *testing.T) {
				options := ImageVerifyOptions{Record: filepath.Join(root, "image.json"), RecordSHA256: recordSHA256, Image: filepath.Join(root, "oci"), BuildRecord: filepath.Join(root, "build.json"), Work: filepath.Join(t.TempDir(), "work"), Check: check}
				change(&options)
				want := map[string]string{"no record digest": "record digest", "no SPDX bundle": "SPDX", "no CycloneDX bundle": "CycloneDX", "no trusted signer": "trusted signer", "no verifier": "verifier"}[name]
				if _, err := VerifyImage(context.Background(), options); err == nil || !strings.Contains(err.Error(), want) {
					t.Fatal("a missing input was not refused by name", err)
				}
			})
		}
	})
}
