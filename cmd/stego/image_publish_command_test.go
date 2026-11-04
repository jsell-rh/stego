package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/appbuild"
)

// These tests run stego image publish (ST-BLD-CLI06) and stego image export
// (ST-BLD-CLI11) as an operator does, with the stego binary of the shared
// fixture, on the image of its one real build, against an in-process TLS
// registry with basic auth.

// publishArgs gives the flags of stego image publish for an image folder,
// with the signature flags that check its signature.
func publishArgs(image, recordSHA256, work string, access, signature []string) []string {
	return append(append([]string{"image", "publish", "--record", filepath.Join(image, "image.json"), "--record-sha256", recordSHA256, "--image", filepath.Join(image, "oci"), "--build-record", filepath.Join(image, "build.json"), "--work", work}, access...), signature...)
}

// registryReceipt reads the publication or retrieval record of a work folder.
func registryReceipt(t *testing.T, work string) appbuild.RegistryImageRecord {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(work, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	var receipt appbuild.RegistryImageRecord
	if err := json.Unmarshal(data, &receipt); err != nil {
		t.Fatal(err)
	}
	return receipt
}

// layerBlob gives the path of the largest blob of an image folder: its layer.
func layerBlob(t *testing.T, image string) string {
	t.Helper()
	folder := filepath.Join(image, "oci", "blobs", "sha256")
	entries, err := os.ReadDir(folder)
	if err != nil {
		t.Fatal(err)
	}
	var largest string
	var size int64
	for _, entry := range entries {
		if info, err := entry.Info(); err == nil && info.Size() > size {
			largest, size = filepath.Join(folder, entry.Name()), info.Size()
		}
	}
	return largest
}

// registryRefusal is one publication or retrieval whose input is refused.
type registryRefusal struct {
	name   string
	change func(args []string) []string
	reason string
}

// registryInputRefusals are the inputs that publication and retrieval
// refuse before they open any connection.
func registryInputRefusals(t *testing.T, registry *registryServer) []registryRefusal {
	t.Helper()
	folder := t.TempDir()
	open := writeFile(t, folder, "open.json", "{\"username\":\"test\",\"password\":\"fixture-credential\"}\n", 0644)
	extra := writeFile(t, folder, "extra.json", "{\"username\":\"test\",\"password\":\"fixture-credential\",\"token\":\"x\"}\n", 0600)
	noPassword := writeFile(t, folder, "no-password.json", "{\"username\":\"test\"}\n", 0600)
	link := filepath.Join(folder, "link.json")
	if err := os.Symlink(registry.credentials, link); err != nil {
		t.Fatal(err)
	}
	set := func(name, value string) func([]string) []string {
		return func(args []string) []string { return setFlag(args, name, value) }
	}
	return []registryRefusal{
		{"a tagged reference", set("repository", registry.repository("application")+":latest"), "without a tag"},
		{"a CA bundle whose digest is not trusted", set("registry-ca-sha256", strings.Repeat("0", 64)), "registry CA digest differs"},
		{"no credentials", func(args []string) []string { return withoutFlag(args, "credentials") }, "requires explicit credentials"},
		{"a missing credential file", set("credentials", filepath.Join(folder, "missing.json")), "cannot open registry credentials"},
		{"a credential file that others can read", set("credentials", open), "private bounded regular file"},
		{"a credential file that is a link", set("credentials", link), "registry credentials"},
		{"a credential file with an extra field", set("credentials", extra), "canonical username and password"},
		{"a credential file with no password", set("credentials", noPassword), "invalid registry credentials"},
		{"a token origin over http", func(args []string) []string { return append(args, "--token-origin", "http://127.0.0.1:1") }, "explicit HTTPS origins"},
		{"a blob origin that is also a token origin", func(args []string) []string {
			return append(args, "--token-origin", "https://auth.example.test", "--blob-origin", "https://auth.example.test")
		}, "must be separate"},
	}
}

func TestImagePublishThroughTheCommand(t *testing.T) {
	t.Run("ST-BLD-UC06-R01-T01 stego image publish pushes only a verified image signed by the trusted signer over TLS, and the registry then holds its exact bytes", func(t *testing.T) {
		t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
		image, _, signature := signedImage(t)
		recordSHA256 := builtImage(t).recordSHA256
		manifest, _ := imageRecordOf(t, image)
		registry := newRegistry(t)
		changed := copyFolder(t, image)
		flipByte(t, layerBlob(t, changed))
		work := filepath.Join(t.TempDir(), "changed")
		_, stderr, err := runStego(t, publishArgs(changed, recordSHA256, work, registry.accessArgs("application"), signature)...)
		if err == nil || !strings.Contains(stderr, "image blob content differs") || registry.requests.Load() != 0 {
			t.Fatalf("stego image publish of an image whose layer differs: %v, %q, %d registry requests; expected a refusal before any request", err, stderr, registry.requests.Load())
		}
		otherSpdx, otherCyclonedx := signatureBundles(t, image, otherSubject)
		missing := filepath.Join(t.TempDir(), "missing")
		for _, item := range []struct {
			name      string
			signature []string
			reason    string
		}{
			{"no signature flags", nil, "image publish requires --spdx-bundle"},
			{"no signature bundles", withSignature(signature, missing+".spdx", missing+".cdx"), "the SPDX signature bundle"},
			{"bundles by another signer", withSignature(signature, otherSpdx, otherCyclonedx), "not by the trusted signer"},
		} {
			work := filepath.Join(t.TempDir(), "unsigned")
			_, stderr, err := runStego(t, publishArgs(image, recordSHA256, work, registry.accessArgs("application"), item.signature)...)
			if err == nil || !strings.Contains(stderr, item.reason) {
				t.Errorf("stego image publish with %s: %v, %q; expected a refusal that names %q", item.name, err, stderr, item.reason)
			}
			if _, err := os.Stat(filepath.Join(work, "registry.json")); !os.IsNotExist(err) {
				t.Errorf("stego image publish with %s wrote a publication record", item.name)
			}
		}
		if registry.requests.Load() != 0 {
			t.Fatalf("the refused publications sent the registry %d requests", registry.requests.Load())
		}
		work = filepath.Join(t.TempDir(), "published")
		stdout := mustStego(t, publishArgs(image, recordSHA256, work, registry.accessArgs("application"), signature)...)
		receipt := registryReceipt(t, work)
		if receipt.Operation != "publish" || receipt.Reference != registry.repository("application")+"@sha256:"+manifest || !receipt.ImageContentsVerified || receipt.ImageRecordSHA256 != recordSHA256 {
			t.Fatalf("the publication record is %+v; expected the image sha256:%s at its explicit place", receipt, manifest)
		}
		var published appbuild.ImagePublished
		lastEvent(t, stdout, &published)
		if published.ImageDigest != manifest {
			t.Fatalf("stego image publish printed %+v; expected the image sha256:%s", published, manifest)
		}
		retrieved := filepath.Join(t.TempDir(), "retrieved")
		mustStego(t, append([]string{"image", "retrieve", "--record", filepath.Join(image, "image.json"), "--record-sha256", recordSHA256, "--build-record", filepath.Join(image, "build.json"), "--work", retrieved}, append(registry.accessArgs("application"), signature...)...)...)
		entries, err := os.ReadDir(filepath.Join(image, "oci", "blobs", "sha256"))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if fileSHA256(t, filepath.Join(retrieved, "oci", "blobs", "sha256", entry.Name())) != entry.Name() {
				t.Errorf("the registry does not hold the exact blob sha256:%s", entry.Name())
			}
		}
	})
	t.Run("ST-BLD-UC06-R01-T02 stego image export writes the same archive each time, never replaces one, and refuses a changed image", func(t *testing.T) {
		fixture := builtImage(t)
		export := func(image, work string) (string, error) {
			_, stderr, err := runStego(t, "image", "export", "--record", filepath.Join(image, "image.json"), "--record-sha256", fixture.recordSHA256, "--image", filepath.Join(image, "oci"), "--build-record", filepath.Join(image, "build.json"), "--work", work)
			return stderr, err
		}
		first, second := filepath.Join(t.TempDir(), "first"), filepath.Join(t.TempDir(), "second")
		for _, work := range []string{first, second} {
			if stderr, err := export(fixture.folder, work); err != nil {
				t.Fatalf("stego image export: %v\n%s", err, stderr)
			}
		}
		archive := fileSHA256(t, filepath.Join(first, "image.docker.tar"))
		if fileSHA256(t, filepath.Join(second, "image.docker.tar")) != archive {
			t.Fatal("two exports of the same image gave different archives")
		}
		if stderr, err := export(fixture.folder, first); err == nil {
			t.Fatalf("stego image export replaced an existing result: %s", stderr)
		}
		if fileSHA256(t, filepath.Join(first, "image.docker.tar")) != archive {
			t.Fatal("a refused export changed the existing archive")
		}
		changed := copyFolder(t, fixture.folder)
		var record appbuild.ImageRecord
		data, err := os.ReadFile(filepath.Join(changed, "image.json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &record); err != nil {
			t.Fatal(err)
		}
		flipByte(t, filepath.Join(changed, "oci", "blobs", "sha256", record.Config.SHA256))
		work := filepath.Join(t.TempDir(), "changed")
		if stderr, err := export(changed, work); err == nil || !strings.Contains(stderr, "image blob content differs") {
			t.Fatalf("stego image export of a changed configuration: %v, %q; expected a refusal", err, stderr)
		}
		for _, name := range []string{"image.docker.tar", "export.json"} {
			if _, err := os.Stat(filepath.Join(work, name)); !os.IsNotExist(err) {
				t.Errorf("a refused export wrote %s", name)
			}
		}
	})
	t.Run("ST-BLD-UC06-R01-T03 stego image publish refuses each bad registry input before anything is pushed", func(t *testing.T) {
		fixture := builtImage(t)
		image, _, signature := signedImage(t)
		registry := newRegistry(t)
		for _, item := range append(registryInputRefusals(t, registry), registryRefusal{"a CA that does not sign the registry", func(args []string) []string {
			ca, caSHA256 := imageCA(t)
			return setFlag(setFlag(args, "registry-ca", ca), "registry-ca-sha256", caSHA256)
		}, "registry"}) {
			work := filepath.Join(t.TempDir(), "work")
			_, stderr, err := runStego(t, item.change(publishArgs(image, fixture.recordSHA256, work, registry.accessArgs("application"), signature))...)
			if err == nil || !strings.Contains(stderr, item.reason) {
				t.Errorf("stego image publish with %s: %v, %q; expected a refusal that names %q", item.name, err, stderr, item.reason)
			}
			if _, err := os.Stat(filepath.Join(work, "registry.json")); !os.IsNotExist(err) {
				t.Errorf("stego image publish with %s wrote a publication record", item.name)
			}
		}
		if registry.requests.Load() != 0 {
			t.Fatalf("the refused publications sent the registry %d requests", registry.requests.Load())
		}
	})
	t.Run("ST-BLD-UC06-R03-T01 stego image publish names a tagged reference, an untrusted CA bundle or bad credentials before it opens any connection", func(t *testing.T) {
		fixture := builtImage(t)
		image, _, signature := signedImage(t)
		registry := newRegistry(t)
		for _, item := range registryInputRefusals(t, registry)[:8] {
			before := registry.requests.Load()
			work := filepath.Join(t.TempDir(), "work")
			_, stderr, err := runStego(t, item.change(publishArgs(image, fixture.recordSHA256, work, registry.accessArgs("application"), signature))...)
			if err == nil || !strings.Contains(stderr, item.reason) {
				t.Errorf("stego image publish with %s: %v, %q; expected a refusal that names %q", item.name, err, stderr, item.reason)
			}
			if registry.requests.Load() != before {
				t.Errorf("stego image publish with %s opened a connection to the registry", item.name)
			}
		}
	})
	t.Run("ST-BLD-UC06-R04-T01 A read-back that differs fails the publication, names both digests, and writes no publication record", func(t *testing.T) {
		fixture := builtImage(t)
		image, _, signature := signedImage(t)
		registry := newRegistry(t)
		registry.tamper.Store(true)
		work := filepath.Join(t.TempDir(), "failed")
		_, stderr, err := runStego(t, publishArgs(image, fixture.recordSHA256, work, registry.accessArgs("application"), signature)...)
		if err == nil || !strings.Contains(stderr, "pushed sha256:"+fixture.manifest) || !strings.Contains(stderr, "expected sha256:"+fixture.manifest) || !strings.Contains(stderr, "read sha256:") {
			t.Fatalf("stego image publish with a registry that gives changed bytes: %v, %q; expected a failure that names the digest pushed and the digest read", err, stderr)
		}
		if _, err := os.Stat(filepath.Join(work, "registry.json")); !os.IsNotExist(err) {
			t.Fatal("a failed publication wrote a publication record")
		}
		registry.tamper.Store(false)
		again := filepath.Join(t.TempDir(), "again")
		mustStego(t, publishArgs(image, fixture.recordSHA256, again, registry.accessArgs("application"), signature)...)
		if receipt := registryReceipt(t, again); receipt.Reference != registry.repository("application")+"@sha256:"+fixture.manifest {
			t.Fatalf("the publication again gave the record %+v", receipt)
		}
	})
}
