package appbuild

import (
	"archive/zip"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/mod/sumdb/dirhash"

	"github.com/jsell-rh/stego/internal/registry"
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
		want := []CachedModule{{Path: "example.test/Upper", Version: "v0.2.0", Sum: upper}, {Path: "example.test/first", Version: "v1.0.0", Sum: first}}
		if len(modules) != 2 || modules[0] != want[0] || modules[1] != want[1] {
			t.Fatalf("the cache lists %v, expected %v", modules, want)
		}
		for index := range want {
			want[index].Source = ProxySource
			modules[index].Source = ProxySource
		}
		record := ModuleCacheRecord{Format: moduleCacheFormat, SourceRevision: strings.Repeat("a", 40), Module: ".", Target: "cmd", GoVersion: GoVersion, GitSHA256: strings.Repeat("b", 64), DownloadCache: Inventory{strings.Repeat("c", 64), 4, 100}, Modules: modules, DependencyProxy: "https://proxy.golang.org"}
		if err := validateModuleCacheRecord(&record); err != nil {
			t.Fatal(err)
		}
		for name, change := range map[string]func(*ModuleCacheRecord){
			"no modules": func(r *ModuleCacheRecord) { r.Modules = nil },
			"bad hash": func(r *ModuleCacheRecord) {
				r.Modules = []CachedModule{{"example.test/first", "v1.0.0", "h1:wrong", ProxySource}}
			},
			"out of order":   func(r *ModuleCacheRecord) { r.Modules = []CachedModule{want[1], want[0]} },
			"repeated":       func(r *ModuleCacheRecord) { r.Modules = []CachedModule{want[0], want[0]} },
			"invalid module": func(r *ModuleCacheRecord) { r.Modules = []CachedModule{{"../escape", "v1.0.0", first, ProxySource}} },
			"no source":      func(r *ModuleCacheRecord) { r.Modules = []CachedModule{{"example.test/first", "v1.0.0", first, ""}} },
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

// runtimeFixture writes a small runtime library folder and gives the go.sum
// hashes of its zip and of its go.mod at version, as stego deps computes
// them (registry.WriteRuntimeArchive).
func runtimeFixture(t *testing.T, folder, version string) (string, string) {
	t.Helper()
	put(t, folder, "go.mod", "module "+registry.RuntimeModule+"\n\ngo 1.26\n")
	put(t, folder, "LICENSE", "license\n")
	put(t, filepath.Join(folder, "probe"), "probe.go", "package probe\n\n// Ready is true.\nconst Ready = true\n")
	archive, err := registry.WriteRuntimeArchive(folder, registry.RuntimeModule, version, filepath.Join(t.TempDir(), "runtime.zip"))
	if err != nil {
		t.Fatal(err)
	}
	return archive.Sum, archive.ModSum
}

func TestRuntimeFolderSuppliesOnlyTheMatchingPin(t *testing.T) {
	const version = "v0.99.0"
	pin := &moduleVersion{registry.RuntimeModule, version}
	t.Run("ST-BLD-UC02-R05-T05 A runtime folder that hashes to the pin is staged in the proxy layout, and no folder or no pin stages nothing", func(t *testing.T) {
		folder := filepath.Join(t.TempDir(), "runtime")
		sum, modSum := runtimeFixture(t, folder, version)
		proxy := filepath.Join(t.TempDir(), "proxy")
		served, err := stageRuntimeFolder(folder, pin, map[moduleVersion]string{*pin: sum}, map[moduleVersion]string{*pin: modSum}, proxy, t.TempDir())
		if err != nil || served == nil || *served != *pin {
			t.Fatalf("the matching folder served %v: %v", served, err)
		}
		versions := filepath.Join(proxy, "github.com", "jsell-rh", "stego", "registry", "runtime", "@v")
		found, err := dirhash.HashZip(filepath.Join(versions, version+".zip"), dirhash.Hash1)
		if err != nil || found != sum {
			t.Fatalf("the served zip hashes to %s, expected %s: %v", found, sum, err)
		}
		mod, err := os.ReadFile(filepath.Join(versions, version+".mod"))
		if err != nil || string(mod) != "module "+registry.RuntimeModule+"\n\ngo 1.26\n" {
			t.Fatalf("the served go.mod is %q: %v", mod, err)
		}
		if served, err := stageRuntimeFolder(folder, nil, nil, nil, filepath.Join(t.TempDir(), "proxy"), t.TempDir()); served != nil || err != nil {
			t.Fatalf("a module that requires no runtime library was served %v: %v", served, err)
		}
		if served, err := stageRuntimeFolder(filepath.Join(t.TempDir(), "none"), pin, nil, nil, filepath.Join(t.TempDir(), "proxy"), t.TempDir()); served != nil || err != nil {
			t.Fatalf("a source with no runtime folder was served %v: %v", served, err)
		}
	})
	t.Run("ST-BLD-UC02-R05-T06 A runtime folder whose zip or go.mod differs from go.sum, or that holds a link or is one, is refused by name, and nothing is staged", func(t *testing.T) {
		folder := filepath.Join(t.TempDir(), "runtime")
		sum, modSum := runtimeFixture(t, folder, version)
		wrong := "h1:" + strings.Repeat("A", 43) + "="
		for name, sums := range map[string][2]string{"zip": {wrong, modSum}, "go.mod": {sum, wrong}} {
			proxy := filepath.Join(t.TempDir(), "proxy")
			served, err := stageRuntimeFolder(folder, pin, map[moduleVersion]string{*pin: sums[0]}, map[moduleVersion]string{*pin: sums[1]}, proxy, t.TempDir())
			if err == nil || served != nil {
				t.Fatalf("a folder whose %s differs from go.sum was served", name)
			}
			for _, part := range []string{registry.RuntimeModule + " " + version, wrong} {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("the refusal of a %s that differs does not name %s: %v", name, part, err)
				}
			}
			if _, statErr := os.Lstat(proxy); !errors.Is(statErr, os.ErrNotExist) {
				t.Errorf("a refused %s left the proxy folder", name)
			}
		}
		served, err := stageRuntimeFolder(folder, pin, map[moduleVersion]string{}, map[moduleVersion]string{*pin: modSum}, filepath.Join(t.TempDir(), "proxy"), t.TempDir())
		if err == nil || served != nil || !strings.Contains(err.Error(), "states no hash") {
			t.Fatalf("a pin that go.sum states no hash for was served %v: %v", served, err)
		}
	})
	t.Run("ST-BLD-UC02-R05-T06 A runtime folder that holds a link, or is a link, is refused by name", func(t *testing.T) {
		outside := t.TempDir()
		put(t, outside, "secret.go", "package probe\n")
		folder := filepath.Join(t.TempDir(), "runtime")
		sum, modSum := runtimeFixture(t, folder, version)
		if err := os.Symlink(filepath.Join(outside, "secret.go"), filepath.Join(folder, "probe", "secret.go")); err != nil {
			t.Fatal(err)
		}
		proxy := filepath.Join(t.TempDir(), "proxy")
		served, err := stageRuntimeFolder(folder, pin, map[moduleVersion]string{*pin: sum}, map[moduleVersion]string{*pin: modSum}, proxy, t.TempDir())
		if err == nil || served != nil || !strings.Contains(err.Error(), "probe/secret.go") {
			t.Fatalf("a folder that holds a link was served %v: %v", served, err)
		}
		linked := filepath.Join(t.TempDir(), "runtime")
		if err := os.Symlink(folder, linked); err != nil {
			t.Fatal(err)
		}
		served, err = stageRuntimeFolder(linked, pin, map[moduleVersion]string{*pin: sum}, map[moduleVersion]string{*pin: modSum}, proxy, t.TempDir())
		if err == nil || served != nil {
			t.Fatalf("a runtime folder that is a link was served %v: %v", served, err)
		}
		if _, statErr := os.Lstat(proxy); !errors.Is(statErr, os.ErrNotExist) {
			t.Error("a refused folder left the proxy folder")
		}
	})
	t.Run("ST-BLD-UC02-R05-T07 The cache record and the build record name the runtime folder as the source of the pin only", func(t *testing.T) {
		sum := "h1:" + strings.Repeat("A", 43) + "="
		record := ModuleCacheRecord{Format: moduleCacheFormat, SourceRevision: strings.Repeat("a", 40), Module: ".", Target: "cmd", GoVersion: GoVersion, GitSHA256: strings.Repeat("b", 64), DownloadCache: Inventory{strings.Repeat("c", 64), 4, 100}, DependencyProxy: ProxySource,
			Modules: []CachedModule{{registry.RuntimeModule, version, sum, RuntimeFolderSource}, {"golang.org/x/text", "v0.1.0", sum, ProxySource}}}
		if err := validateModuleCacheRecord(&record); err != nil {
			t.Fatal(err)
		}
		changed := record
		changed.Modules = []CachedModule{record.Modules[0], {"golang.org/x/text", "v0.1.0", sum, RuntimeFolderSource}}
		if err := validateModuleCacheRecord(&changed); err == nil {
			t.Fatal("a cache record that names the runtime folder as the source of another module was accepted")
		}
		modules := []Module{{Path: registry.RuntimeModule, Version: version, Sum: sum, GoModSum: sum}, {Path: "golang.org/x/text", Version: "v0.1.0", Sum: sum, GoModSum: sum}, {Path: "example.test/main", LocalPath: "."}}
		recordModuleSources(modules, &record)
		if modules[0].Source != RuntimeFolderSource || modules[1].Source != "" || modules[2].Source != "" {
			t.Fatalf("the build record names the sources %q, %q and %q; expected only the runtime pin from %s", modules[0].Source, modules[1].Source, modules[2].Source, RuntimeFolderSource)
		}
		for _, module := range modules {
			if err := validateModule(module, false); err != nil {
				t.Fatalf("module %s: %v", module.Path, err)
			}
		}
		other := modules[1]
		other.Source = RuntimeFolderSource
		if err := validateModule(other, false); err == nil {
			t.Fatal("a build record that names the runtime folder as the source of another module was accepted")
		}
		changedSum := []Module{{Path: registry.RuntimeModule, Version: version, Sum: "h1:" + strings.Repeat("B", 43) + "=", GoModSum: sum}}
		recordModuleSources(changedSum, &record)
		if changedSum[0].Source != "" {
			t.Fatal("a module whose hash differs from the cache record was named as from the runtime folder")
		}
	})
}
