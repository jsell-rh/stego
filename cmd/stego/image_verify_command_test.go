package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests run stego image verify (ST-BLD-CLI09) as a consumer does, with
// the stego binary of the shared fixture, on the image of its one real build.

// withSignature gives a copy of signature flags with the two bundles changed.
func withSignature(signature []string, spdx, cyclonedx string) []string {
	changed := append([]string{}, signature...)
	changed[1], changed[3] = spdx, cyclonedx
	return changed
}

// withoutFlag gives a copy of the flags without one flag and its value.
func withoutFlag(flags []string, name string) []string {
	var kept []string
	for index := 0; index < len(flags); index += 2 {
		if flags[index] != "--"+name {
			kept = append(kept, flags[index], flags[index+1])
		}
	}
	return kept
}

// flipByte changes one byte in the middle of a file.
func flipByte(t *testing.T, name string) {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 0xff
	if err := os.WriteFile(name, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestImageVerifyThroughTheCommand(t *testing.T) {
	t.Run("ST-BLD-UC04-R04-T01 stego image verify reports a match only when the record, the trusted build record and the trusted signature all pass", func(t *testing.T) {
		image, _, signature := signedImage(t)
		recordSHA256 := builtImage(t).recordSHA256
		verify := func(image string, signature []string) (string, error) {
			work := filepath.Join(t.TempDir(), "work")
			args := append([]string{"image", "verify", "--record", filepath.Join(image, "image.json"), "--record-sha256", recordSHA256, "--image", filepath.Join(image, "oci"), "--build-record", filepath.Join(image, "build.json"), "--work", work}, signature...)
			_, stderr, err := runStego(t, args...)
			return stderr, err
		}
		if stderr, err := verify(image, signature); err != nil {
			t.Fatalf("stego image verify refused the signed image of its trusted build: %v\n%s", err, stderr)
		}
		changedLayer := copyFolder(t, image)
		var layer string
		entries, err := os.ReadDir(filepath.Join(changedLayer, "oci", "blobs", "sha256"))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if info, err := entry.Info(); err == nil && info.Size() > 1<<20 {
				layer = filepath.Join(changedLayer, "oci", "blobs", "sha256", entry.Name())
			}
		}
		if layer == "" {
			t.Fatal("the image holds no layer blob")
		}
		flipByte(t, layer)
		changedBuild := copyFolder(t, image)
		flipByte(t, filepath.Join(changedBuild, "build.json"))
		otherSpdx, otherCyclonedx := signatureBundles(t, image, otherSubject)
		missing := filepath.Join(t.TempDir(), "missing")
		for _, item := range []struct {
			name      string
			image     string
			signature []string
			reason    string
		}{
			{"an image whose layer differs from its record", changedLayer, signature, "image blob content differs"},
			{"a build record that is not the trusted one", changedBuild, signature, "captured image input differs"},
			{"bundles by another signer", image, withSignature(signature, otherSpdx, otherCyclonedx), "not by the trusted signer"},
			{"no signature bundles", image, withSignature(signature, missing+".spdx", missing+".cdx"), "the SPDX signature bundle"},
		} {
			stderr, err := verify(item.image, item.signature)
			if err == nil || !strings.Contains(stderr, item.reason) {
				t.Errorf("stego image verify of %s: %v, %q; expected a failed check that names %q", item.name, err, stderr, item.reason)
			}
		}
		for _, name := range signatureNames {
			stderr, err := verify(image, withoutFlag(signature, name))
			if err == nil || !strings.Contains(stderr, "image verify requires --"+name) {
				t.Errorf("stego image verify without --%s: %v, %q; expected a refusal by name", name, err, stderr)
			}
		}
	})
}
