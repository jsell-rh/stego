package appbuild

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/mod/sumdb/dirhash"
)

// putModuleZip writes a module zip into a download cache and returns its
// hash as go.sum states it.
func putModuleZip(t *testing.T, download, escapedPath, version, content string) string {
	t.Helper()
	folder := filepath.Join(download, filepath.FromSlash(escapedPath), "@v")
	if err := os.MkdirAll(folder, 0700); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(folder, version+".zip")
	file, err := os.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	entry, err := writer.Create(escapedPath + "@" + version + "/module.go")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	put(t, folder, version+".mod", "module "+escapedPath+"\n")
	sum, err := dirhash.HashZip(name, dirhash.Hash1)
	if err != nil {
		t.Fatal(err)
	}
	return sum
}

func TestModuleCacheHoldsOnlyStatedZips(t *testing.T) {
	t.Run("ST-BLD-INV03-T A cache holds only module zips whose hashes go.sum states, and its record lists each", func(t *testing.T) {
		download := t.TempDir()
		first := putModuleZip(t, download, "example.test/first", "v1.0.0", "package first\n")
		upper := putModuleZip(t, download, "example.test/!upper", "v0.2.0", "package upper\n")
		sums := map[moduleVersion]string{{"example.test/first", "v1.0.0"}: first, {"example.test/Upper", "v0.2.0"}: upper}
		modules, err := checkCachedModules(download, sums)
		if err != nil {
			t.Fatal(err)
		}
		want := []CachedModule{{"example.test/Upper", "v0.2.0", upper}, {"example.test/first", "v1.0.0", first}}
		if len(modules) != 2 || modules[0] != want[0] || modules[1] != want[1] {
			t.Fatalf("the cache lists %v, expected %v", modules, want)
		}
		record := ModuleCacheRecord{Format: moduleCacheFormat, SourceRevision: strings.Repeat("a", 40), Module: ".", Target: "cmd", GoVersion: GoVersion, GitSHA256: strings.Repeat("b", 64), DownloadCache: Inventory{strings.Repeat("c", 64), 4, 100}, Modules: modules, DependencyProxy: "https://proxy.golang.org"}
		if err := validateModuleCacheRecord(&record); err != nil {
			t.Fatal(err)
		}
		for name, change := range map[string]func(*ModuleCacheRecord){
			"no modules":     func(r *ModuleCacheRecord) { r.Modules = nil },
			"bad hash":       func(r *ModuleCacheRecord) { r.Modules = []CachedModule{{"example.test/first", "v1.0.0", "h1:wrong"}} },
			"out of order":   func(r *ModuleCacheRecord) { r.Modules = []CachedModule{want[1], want[0]} },
			"repeated":       func(r *ModuleCacheRecord) { r.Modules = []CachedModule{want[0], want[0]} },
			"invalid module": func(r *ModuleCacheRecord) { r.Modules = []CachedModule{{"../escape", "v1.0.0", first}} },
		} {
			changed := record
			change(&changed)
			if err := validateModuleCacheRecord(&changed); err == nil {
				t.Errorf("a cache record with %s was accepted", name)
			}
		}
		delete(sums, moduleVersion{"example.test/first", "v1.0.0"})
		if _, err := checkCachedModules(download, sums); err == nil || !strings.Contains(err.Error(), "example.test/first v1.0.0") {
			t.Fatalf("a zip that go.sum does not state was accepted, or not named: %v", err)
		}
		sums[moduleVersion{"example.test/first", "v1.0.0"}] = upper
		_, err = checkCachedModules(download, sums)
		if err == nil || !strings.Contains(err.Error(), "example.test/first v1.0.0") || !strings.Contains(err.Error(), upper) || !strings.Contains(err.Error(), first) {
			t.Fatalf("a zip whose hash differs was accepted, or its hashes not named: %v", err)
		}
	})
}

func TestReadSumsKeepsZipHashesOnly(t *testing.T) {
	root := t.TempDir()
	sum := "h1:" + strings.Repeat("A", 43) + "="
	put(t, root, "go.sum", "example.test/a v1.0.0 "+sum+"\nexample.test/a v1.0.0/go.mod "+sum+"\nexample.test/b v1.0.0/go.mod "+sum+"\n")
	sums, err := readSums(filepath.Join(root, "go.sum"))
	if err != nil || len(sums) != 1 || sums[moduleVersion{"example.test/a", "v1.0.0"}] != sum {
		t.Fatal(sums, err)
	}
	put(t, root, "go.sum", "example.test/a v1.0.0\n")
	if _, err := readSums(filepath.Join(root, "go.sum")); err == nil {
		t.Fatal("a malformed go.sum was accepted")
	}
}
