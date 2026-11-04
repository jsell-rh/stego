package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests run stego image retrieve (ST-BLD-CLI10) as a consumer does,
// with the stego binary of the shared fixture, on the signed image of its one
// real build, against an in-process TLS registry with basic auth.

// retrieveArgs gives the flags of stego image retrieve for an image folder.
func retrieveArgs(image, recordSHA256, work string, access, signature []string) []string {
	return append(append([]string{"image", "retrieve", "--record", filepath.Join(image, "image.json"), "--record-sha256", recordSHA256, "--build-record", filepath.Join(image, "build.json"), "--work", work}, access...), signature...)
}

// noImageWritten fails the test when a work folder holds an image or a
// retrieval record.
func noImageWritten(t *testing.T, label, work string) {
	t.Helper()
	for _, name := range []string{"oci", "unverified-oci", "registry.json"} {
		if _, err := os.Stat(filepath.Join(work, name)); !os.IsNotExist(err) {
			t.Errorf("stego image retrieve with %s left %s", label, name)
		}
	}
}

func TestImageRetrieveThroughTheCommand(t *testing.T) {
	t.Run("ST-BLD-UC07-R01-T01 stego image retrieve checks the pulled image against its record, its build record and its signature before it writes it", func(t *testing.T) {
		image, _, signature := signedImage(t)
		recordSHA256 := builtImage(t).recordSHA256
		manifest, _ := imageRecordOf(t, image)
		registry := newRegistry(t)
		access := registry.accessArgs("application")
		mustStego(t, publishArgs(image, recordSHA256, filepath.Join(t.TempDir(), "published"), access, signature)...)
		work := filepath.Join(t.TempDir(), "retrieved")
		mustStego(t, retrieveArgs(image, recordSHA256, work, access, signature)...)
		receipt := registryReceipt(t, work)
		if receipt.Operation != "retrieve" || !receipt.ImageContentsVerified || receipt.Signature == nil || receipt.Signature.ImageSHA256 != manifest || receipt.Signature.Signer.Subject != trustedSubject {
			t.Fatalf("the retrieval record is %+v; expected a verified image with the trusted signature of sha256:%s", receipt, manifest)
		}
		if fileSHA256(t, filepath.Join(work, "oci", "blobs", "sha256", manifest)) != manifest {
			t.Fatal("the retrieved image does not hold the exact manifest")
		}
		registry.tamper.Store(true)
		work = filepath.Join(t.TempDir(), "changed")
		_, stderr, err := runStego(t, retrieveArgs(image, recordSHA256, work, access, signature)...)
		if err == nil || !strings.Contains(stderr, "expected sha256:"+manifest) {
			t.Errorf("stego image retrieve of changed bytes: %v, %q; expected a refusal that names the expected manifest digest", err, stderr)
		}
		noImageWritten(t, "changed bytes", work)
		registry.tamper.Store(false)
		otherSpdx, otherCyclonedx := signatureBundles(t, image, otherSubject)
		work = filepath.Join(t.TempDir(), "other-signer")
		_, stderr, err = runStego(t, retrieveArgs(image, recordSHA256, work, access, withSignature(signature, otherSpdx, otherCyclonedx))...)
		if err == nil || !strings.Contains(stderr, "not by the trusted signer") {
			t.Errorf("stego image retrieve with bundles by another signer: %v, %q; expected a refusal that names the trusted signer", err, stderr)
		}
		noImageWritten(t, "bundles by another signer", work)
	})
	t.Run("ST-BLD-UC07-R02-T01 stego image retrieve names each missing signature input and each bad registry input before it opens any connection", func(t *testing.T) {
		image, _, signature := signedImage(t)
		recordSHA256 := builtImage(t).recordSHA256
		registry := newRegistry(t)
		access := registry.accessArgs("application")
		for _, name := range signatureNames {
			work := filepath.Join(t.TempDir(), "work")
			_, stderr, err := runStego(t, retrieveArgs(image, recordSHA256, work, access, withoutFlag(signature, name))...)
			if err == nil || !strings.Contains(stderr, "image retrieve requires --"+name) {
				t.Errorf("stego image retrieve without --%s: %v, %q; expected a refusal by name", name, err, stderr)
			}
			noImageWritten(t, "no --"+name, work)
		}
		for _, item := range registryInputRefusals(t, registry)[:8] {
			work := filepath.Join(t.TempDir(), "work")
			args := retrieveArgs(image, recordSHA256, work, nil, signature)
			args = item.change(append(args, access...))
			_, stderr, err := runStego(t, args...)
			if err == nil || !strings.Contains(stderr, item.reason) {
				t.Errorf("stego image retrieve with %s: %v, %q; expected a refusal that names %q", item.name, err, stderr, item.reason)
			}
			noImageWritten(t, item.name, work)
		}
		if registry.requests.Load() != 0 {
			t.Fatalf("the refused retrievals sent the registry %d requests", registry.requests.Load())
		}
	})
}
