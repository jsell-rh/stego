package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jsell-rh/stego/internal/appbuild"
	"github.com/jsell-rh/stego/internal/compiler"
	"github.com/jsell-rh/stego/internal/gen"
)

// These tests run stego build and its subcommands in process, as an operator
// does. A download or a build needs the Go SDK that STEGO pins: the tests use
// STEGO_TEST_PINNED_GO, else bin/go of the SDK that runs them, and skip only
// when STEGO refuses that SDK as not the pinned one. A download reaches the
// public module proxy.

const tinyModule = "github.com/coder/websocket"
const tinyVersion = "v1.8.15"
const tinySum = "h1:6B2JPeOGlpff2Uz6vOEH1Vzpi0iUz20A+lPVhPHtNUA="
const tinyModSum = "h1:NX3SzP+inril6yawo5CQXx8+fk145lPDC6pumgx0mVg="

func pinnedGo(t *testing.T) string {
	t.Helper()
	if name := os.Getenv("STEGO_TEST_PINNED_GO"); name != "" {
		return name
	}
	return filepath.Join(runtime.GOROOT(), "bin", "go")
}

func skipUnlessPinned(t *testing.T, err error) {
	t.Helper()
	if err != nil && strings.Contains(err.Error(), "is not the pinned official release") {
		t.Skipf("the Go SDK of this run is not the pinned one; set STEGO_TEST_PINNED_GO: %v", err)
	}
}

func git(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-c", "user.name=STEGO", "-c", "user.email=stego@example.test", "-c", "commit.gpgsign=false", "-c", "init.defaultBranch=main"}, args...)...)
	command.Dir = directory
	command.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

// tinySource commits a module with one small dependency and returns the
// repository and its revision. The files map adds or replaces files.
func tinySource(t *testing.T, files map[string]string) (string, string) {
	t.Helper()
	repository := filepath.Join(t.TempDir(), "source")
	content := map[string]string{
		"go.mod":           "module example.com/tiny\n\ngo 1.26.10\n\nrequire " + tinyModule + " " + tinyVersion + "\n",
		"go.sum":           tinyModule + " " + tinyVersion + " " + tinySum + "\n" + tinyModule + " " + tinyVersion + "/go.mod " + tinyModSum + "\n",
		"cmd/tiny/main.go": "package main\n\nimport _ \"" + tinyModule + "\"\n\nfunc main() {}\n",
	}
	for name, data := range files {
		content[name] = data
	}
	for name, data := range content {
		full := filepath.Join(repository, name)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(data), 0644); err != nil {
			t.Fatal(err)
		}
	}
	git(t, repository, "init", "-q")
	git(t, repository, "add", "-A")
	git(t, repository, "commit", "-q", "-m", "source")
	return repository, git(t, repository, "rev-parse", "HEAD")
}

// folderNames lists the names in a folder.
func folderNames(t *testing.T, folder string) []string {
	t.Helper()
	entries, err := os.ReadDir(folder)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func downloadArgs(source, revision, goTool, output string) []string {
	return []string{"download", "--source", source, "--revision", revision, "--target", "cmd/tiny", "--go", goTool, "--output", output}
}

func readCacheRecord(t *testing.T, output string) appbuild.ModuleCacheRecord {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(output, "module-cache.json"))
	if err != nil {
		t.Fatal(err)
	}
	var record appbuild.ModuleCacheRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	return record
}

func cachedZips(t *testing.T, output string) []string {
	t.Helper()
	var zips []string
	download := filepath.Join(output, "cache", "download")
	if err := filepath.WalkDir(download, func(name string, entry os.DirEntry, err error) error {
		if err == nil && strings.HasSuffix(name, ".zip") {
			relative, _ := filepath.Rel(download, name)
			zips = append(zips, filepath.ToSlash(relative))
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return zips
}

func TestBuildDownloadWritesAStatedRecordedCache(t *testing.T) {
	t.Run("ST-BLD-UC02-R01-T01 A downloaded cache holds only the zips whose hashes go.sum states", func(t *testing.T) {
		source, revision := tinySource(t, nil)
		output := filepath.Join(t.TempDir(), "cache")
		_, err := capture(t, runBuild, downloadArgs(source, revision, pinnedGo(t), output)...)
		skipUnlessPinned(t, err)
		if err != nil {
			t.Fatal(err)
		}
		if zips := cachedZips(t, output); len(zips) != 1 || zips[0] != tinyModule+"/@v/"+tinyVersion+".zip" {
			t.Fatalf("the cache holds the zips %v; expected only the zip of %s %s", zips, tinyModule, tinyVersion)
		}
	})
	t.Run("ST-BLD-UC02-R01-T02 A zip whose hash differs from go.sum stops the download, which names both hashes", func(t *testing.T) {
		wrong := "h1:" + strings.Repeat("A", 43) + "="
		source, revision := tinySource(t, map[string]string{"go.sum": tinyModule + " " + tinyVersion + " " + wrong + "\n" + tinyModule + " " + tinyVersion + "/go.mod " + tinyModSum + "\n"})
		parent := t.TempDir()
		output := filepath.Join(parent, "cache")
		_, err := capture(t, runBuild, downloadArgs(source, revision, pinnedGo(t), output)...)
		skipUnlessPinned(t, err)
		if err == nil {
			t.Fatal("a download with a zip that differs from go.sum succeeded")
		}
		for _, part := range []string{tinyModule + " " + tinyVersion, wrong, tinySum} {
			if !strings.Contains(err.Error(), part) {
				t.Errorf("the refusal does not name %s: %v", part, err)
			}
		}
		if names := folderNames(t, parent); len(names) != 0 {
			t.Fatalf("a refused download left %v", names)
		}
	})
	t.Run("ST-BLD-UC02-R02-T01 The cache record lists each module by path, version and hash, and the revision it serves", func(t *testing.T) {
		source, revision := tinySource(t, nil)
		output := filepath.Join(t.TempDir(), "cache")
		_, err := capture(t, runBuild, downloadArgs(source, revision, pinnedGo(t), output)...)
		skipUnlessPinned(t, err)
		if err != nil {
			t.Fatal(err)
		}
		record := readCacheRecord(t, output)
		want := appbuild.CachedModule{Path: tinyModule, Version: tinyVersion, Sum: tinySum}
		if record.SourceRevision != revision || len(record.Modules) != 1 || record.Modules[0] != want {
			t.Fatalf("the record names revision %s and modules %v; expected %s and %v", record.SourceRevision, record.Modules, revision, want)
		}
	})
}

func TestBuildDownloadLeavesNothingWhenItFails(t *testing.T) {
	t.Run("ST-BLD-UC02-R03-T01 A module the proxy cannot supply stops the download, which names it and leaves nothing", func(t *testing.T) {
		missing := "example.invalid/stego-missing"
		sum := "h1:" + strings.Repeat("A", 42) + "E="
		source, revision := tinySource(t, map[string]string{
			"go.mod":           "module example.com/tiny\n\ngo 1.26.10\n\nrequire " + missing + " v1.0.0\n",
			"go.sum":           missing + " v1.0.0 " + sum + "\n" + missing + " v1.0.0/go.mod " + sum + "\n",
			"cmd/tiny/main.go": "package main\n\nimport _ \"" + missing + "\"\n\nfunc main() {}\n",
		})
		parent := t.TempDir()
		_, err := capture(t, runBuild, downloadArgs(source, revision, pinnedGo(t), filepath.Join(parent, "cache"))...)
		skipUnlessPinned(t, err)
		if err == nil || !strings.Contains(err.Error(), missing+" v1.0.0") {
			t.Fatalf("the download did not fail naming %s v1.0.0: %v", missing, err)
		}
		if names := folderNames(t, parent); len(names) != 0 {
			t.Fatalf("a failed download left %v", names)
		}
	})
	t.Run("ST-BLD-UC02-R03-T02 An interrupted download leaves no cache folder and no record", func(t *testing.T) {
		source, revision := tinySource(t, nil)
		goTool := pinnedGo(t)
		guard := make(chan os.Signal, 1)
		signal.Notify(guard, os.Interrupt)
		defer signal.Stop(guard)
		interrupted := 0
		for _, delay := range []time.Duration{0, 10 * time.Millisecond, 50 * time.Millisecond, 200 * time.Millisecond, 500 * time.Millisecond, time.Second} {
			parent := t.TempDir()
			output := filepath.Join(parent, "cache")
			timer := time.AfterFunc(delay, func() { _ = syscall.Kill(os.Getpid(), syscall.SIGINT) })
			_, err := capture(t, runBuild, downloadArgs(source, revision, goTool, output)...)
			timer.Stop()
			time.Sleep(10 * time.Millisecond)
			for len(guard) > 0 {
				<-guard
			}
			skipUnlessPinned(t, err)
			names := folderNames(t, parent)
			if err == nil {
				if len(names) != 1 || names[0] != "cache" {
					t.Fatalf("a complete download left %v", names)
				}
				readCacheRecord(t, output)
				continue
			}
			interrupted++
			if len(names) != 0 {
				t.Fatalf("a download interrupted after %s left %v: %v", delay, names, err)
			}
		}
		if interrupted == 0 {
			t.Fatal("no interrupt reached a download; the test shows nothing")
		}
		t.Logf("%d of the downloads were interrupted", interrupted)
	})
}

func TestBuildDownloadChecksTargetAndSDKFirst(t *testing.T) {
	t.Run("ST-BLD-UC02-R04-T01 A target that exists or overlaps, or an SDK that is not pinned, is refused by name before any work", func(t *testing.T) {
		root := t.TempDir()
		source := filepath.Join(root, "source")
		fakeSDK := filepath.Join(root, "sdk")
		for _, name := range []string{source, filepath.Join(fakeSDK, "bin"), filepath.Join(root, "exists")} {
			if err := os.MkdirAll(name, 0755); err != nil {
				t.Fatal(err)
			}
		}
		fakeGo := filepath.Join(fakeSDK, "bin", "go")
		if err := os.WriteFile(fakeGo, []byte("not executed\n"), 0755); err != nil {
			t.Fatal(err)
		}
		revision := strings.Repeat("a", 40)
		for name, testCase := range map[string]struct {
			output string
			names  []string
		}{
			"an existing target":        {filepath.Join(root, "exists"), []string{filepath.Join(root, "exists"), "exists", "new folder"}},
			"a target in the source":    {filepath.Join(source, "cache"), []string{filepath.Join(source, "cache"), "inside the source " + source}},
			"a target in the SDK":       {filepath.Join(fakeSDK, "cache"), []string{filepath.Join(fakeSDK, "cache"), "inside the Go SDK " + fakeSDK}},
			"an SDK that is not pinned": {filepath.Join(root, "cache"), []string{fakeSDK, "is not the pinned official release", "found files=1", "expected files="}},
		} {
			t.Run(name, func(t *testing.T) {
				before := folderNames(t, root)
				_, err := capture(t, runBuild, downloadArgs(source, revision, fakeGo, testCase.output)...)
				if err == nil {
					t.Fatal("the download was not refused")
				}
				for _, part := range testCase.names {
					if !strings.Contains(err.Error(), part) {
						t.Errorf("the refusal does not name %q: %v", part, err)
					}
				}
				// The work folder comes before any command, so no network
				// connection was opened when it does not exist.
				if after := folderNames(t, root); strings.Join(after, " ") != strings.Join(before, " ") {
					t.Fatalf("a refused download changed the folder: %v, then %v", before, after)
				}
			})
		}
	})
}

// buildGenerator writes one generated program that needs the small module.
type buildGenerator struct{ files []gen.File }

func (g buildGenerator) Generate(gen.Context) ([]gen.File, *gen.Wiring, error) {
	return g.files, &gen.Wiring{}, nil
}

// buildSource commits a project that the compiler applied, with a recorded
// generation, whose generated program out/internal/api/tiny needs the small
// module. It returns the repository and its revision.
func buildSource(t *testing.T) (string, string) {
	t.Helper()
	return buildSourceIn(t, t.TempDir())
}

// buildSourceIn commits the project of buildSource in the folder root.
func buildSourceIn(t *testing.T, root string) (string, string) {
	t.Helper()
	project := filepath.Join(root, "source")
	registry := filepath.Join(root, "registry")
	files := map[string]string{
		filepath.Join(project, "service.yaml"):                                "kind: service\nname: tiny\narchetype: tiny-arch\nlanguage: go\nentities:\n  - name: Widget\n    fields:\n      - { name: label, type: string }\ncollections:\n  widgets:\n    entity: Widget\n    operations: [create, read]\n",
		filepath.Join(registry, "archetypes", "tiny-arch", "archetype.yaml"):  "kind: archetype\nname: tiny-arch\nlanguage: go\nversion: 1.0.0\ncomponents:\n  - tiny-api\n  - tiny-store\nconventions:\n  layout: flat\n  error_handling: problem-details-rfc\n  logging: structured-json\n  test_pattern: table-driven\nbindings:\n  storage-adapter: tiny-store\n",
		filepath.Join(registry, "components", "tiny-api", "component.yaml"):   "kind: component\nname: tiny-api\nversion: 1.0.0\noutput_namespace: internal/api\nrequires:\n  - storage-adapter\nprovides:\n  - http-server\nslots: []\n",
		filepath.Join(registry, "components", "tiny-store", "component.yaml"): "kind: component\nname: tiny-store\nversion: 1.0.0\noutput_namespace: internal/storage\nrequires: []\nprovides:\n  - storage-adapter\nslots: []\n",
	}
	for name, data := range files {
		writeProjectFile(t, name, data)
	}
	input := compiler.ReconcilerInput{
		ProjectDir: project, RegistryDir: registry, ModuleName: "example.com/tiny", GoVersion: "1.26.10",
		Generators: map[string]gen.Generator{
			"tiny-api":   buildGenerator{[]gen.File{{Path: "internal/api/tiny/main.go", Content: []byte("package main\n\nimport _ \"" + tinyModule + "\"\n\nfunc main() {}\n")}}},
			"tiny-store": buildGenerator{},
		},
	}
	apply := func() {
		t.Helper()
		plan, err := compiler.Reconcile(input)
		if err != nil {
			t.Fatal(err)
		}
		if err := compiler.Apply(plan, project, ""); err != nil {
			t.Fatal(err)
		}
	}
	apply()
	module, err := os.ReadFile(filepath.Join(project, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	writeProjectFile(t, filepath.Join(project, "go.mod"), string(module)+"\nrequire "+tinyModule+" "+tinyVersion+"\n")
	writeProjectFile(t, filepath.Join(project, "go.sum"), tinyModule+" "+tinyVersion+" "+tinySum+"\n"+tinyModule+" "+tinyVersion+"/go.mod "+tinyModSum+"\n")
	apply()
	git(t, project, "init", "-q")
	git(t, project, "add", "-A")
	git(t, project, "commit", "-q", "-m", "source")
	return project, git(t, project, "rev-parse", "HEAD")
}

func buildArgs(source, revision, goTool, work, output, cache string) []string {
	return []string{"--source", source, "--revision", revision, "--target", "out/internal/api/tiny", "--go", goTool, "--work", work, "--output", output, "--module-cache", cache}
}

func fileSHA256(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestBuildIsReproducibleAndRecordsEveryInput(t *testing.T) {
	source, revision := buildSource(t)
	goTool := pinnedGo(t)
	root := t.TempDir()
	// A build keeps its work folders, which hold the read-only module
	// sources Go extracts. Make them writable so the folder can be removed.
	t.Cleanup(func() {
		_ = filepath.WalkDir(root, func(name string, entry os.DirEntry, err error) error {
			if err == nil && entry.IsDir() {
				_ = os.Chmod(name, 0700)
			}
			return nil
		})
	})
	cache := filepath.Join(root, "cache")
	_, err := capture(t, runBuild, []string{"download", "--source", source, "--revision", revision, "--target", "out/internal/api/tiny", "--go", goTool, "--output", cache}...)
	skipUnlessPinned(t, err)
	if err != nil {
		t.Fatal(err)
	}
	// A credential in the caller's environment must not reach the record.
	t.Setenv("GH_TOKEN", "stego-test-credential")
	t.Setenv("GOFLAGS", "-race")
	results := []string{filepath.Join(root, "first"), filepath.Join(root, "second")}
	for index, result := range results {
		if _, err := capture(t, runBuild, buildArgs(source, revision, goTool, filepath.Join(root, "work"+string(rune('a'+index))), result, cache)...); err != nil {
			t.Fatalf("build %d: %v", index+1, err)
		}
	}
	// brokenCache copies the cache, with its one zip removed or changed, and
	// runs a build from the copy.
	brokenCache := func(t *testing.T, change string) (error, []string) {
		t.Helper()
		parent := t.TempDir()
		copied := filepath.Join(parent, "cache")
		if output, err := exec.Command("cp", "-a", cache, copied).CombinedOutput(); err != nil {
			t.Fatalf("copy: %v\n%s", err, output)
		}
		zip := filepath.Join(copied, "cache", "download", tinyModule, "@v", tinyVersion+".zip")
		var err error
		if change == "missing" {
			err = os.Remove(zip)
		} else {
			var data []byte
			if data, err = os.ReadFile(zip); err == nil {
				data[len(data)/2] ^= 0xff
				err = os.WriteFile(zip, data, 0600)
			}
		}
		if err != nil {
			t.Fatal(err)
		}
		_, err = capture(t, runBuild, buildArgs(source, revision, goTool, filepath.Join(parent, "work"), filepath.Join(parent, "result"), copied)...)
		names := folderNames(t, parent)
		return err, names
	}
	t.Run("ST-BLD-INV04-T A build runs with the proxy off from its recorded cache, and fails when a module is missing or differs", func(t *testing.T) {
		var record appbuild.Record
		data, err := os.ReadFile(filepath.Join(results[0], "build.json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &record); err != nil {
			t.Fatal(err)
		}
		if record.DependencyProxy != "off" || record.Environment["GOPROXY"] != "off" || record.ModuleCache == nil || *record.ModuleCache != readCacheRecord(t, cache).DownloadCache {
			t.Fatalf("the record does not show an offline build from the recorded cache: proxy %q, cache %v", record.DependencyProxy, record.ModuleCache)
		}
		for _, change := range []string{"missing", "changed"} {
			if err, _ := brokenCache(t, change); err == nil {
				t.Errorf("a build from a cache whose zip is %s succeeded", change)
			}
		}
	})
	t.Run("ST-BLD-UC03-R01-T02 A build from a cache whose module zip is missing or changed fails and leaves no result", func(t *testing.T) {
		for _, change := range []string{"missing", "changed"} {
			err, names := brokenCache(t, change)
			if err == nil || !strings.Contains(err.Error(), "module cache") {
				t.Errorf("a build from a cache whose zip is %s did not fail naming the module cache: %v", change, err)
			}
			if strings.Contains(strings.Join(names, " "), "result") {
				t.Errorf("a failed build left a result folder: %v", names)
			}
		}
	})
	t.Run("ST-BLD-INV06-T Two builds from the same inputs give byte-identical executables, records and SBOMs", func(t *testing.T) {
		for _, name := range []string{"application", "build.json", "sbom.spdx.json", "sbom.cdx.json"} {
			if fileSHA256(t, filepath.Join(results[0], name)) != fileSHA256(t, filepath.Join(results[1], name)) {
				t.Errorf("two builds from the same inputs gave a different %s", name)
			}
		}
	})
	t.Run("ST-BLD-UC03-R04-T01 The record names every input and output, and both SBOMs are written from it", func(t *testing.T) {
		data, err := os.ReadFile(filepath.Join(results[0], "build.json"))
		if err != nil {
			t.Fatal(err)
		}
		var record appbuild.Record
		if err := json.Unmarshal(data, &record); err != nil {
			t.Fatal(err)
		}
		if record.SourceRevision != revision || record.Toolchain.Files < 1 || record.ModuleCache == nil || len(record.BuildFlags) == 0 || record.GenerationStateSHA256 == "" || record.Artifact.SHA256 != fileSHA256(t, filepath.Join(results[0], "application")) {
			t.Fatalf("the record does not name the revision, the SDK, the cache, the settings, the generation record and the executable: %+v", record)
		}
		if strings.Join(record.SBOMs, " ") != "sbom.spdx.json sbom.cdx.json" {
			t.Fatalf("the record names the SBOMs %v; expected an SPDX and a CycloneDX SBOM", record.SBOMs)
		}
		sboms, err := appbuild.WriteSBOMs(&record)
		if err != nil {
			t.Fatal(err)
		}
		sums, err := os.ReadFile(filepath.Join(results[0], "SHA256SUMS"))
		if err != nil {
			t.Fatal(err)
		}
		for _, sbom := range sboms {
			name := sbom.Format.FileName()
			written, err := os.ReadFile(filepath.Join(results[0], name))
			if err != nil || string(written) != string(sbom.Document) {
				t.Errorf("the result's %s is not the SBOM the record gives: %v", name, err)
			}
			if !strings.Contains(string(sums), sbom.Digest+"  "+name+"\n") {
				t.Errorf("SHA256SUMS does not list %s", name)
			}
		}
	})
	t.Run("ST-BLD-UC03-R03-T02 Builds from the same inputs give the same executable and record", func(t *testing.T) {
		for _, name := range []string{"application", "build.json", "sbom.spdx.json", "sbom.cdx.json", "SHA256SUMS"} {
			if fileSHA256(t, filepath.Join(results[0], name)) != fileSHA256(t, filepath.Join(results[1], name)) {
				t.Errorf("a later build from the same inputs gave a different %s", name)
			}
		}
	})
	t.Run("ST-BLD-INV07-T The record of a real build names every input and the fixed build settings, and no credential", func(t *testing.T) {
		raw, err := os.ReadFile(filepath.Join(results[0], "build.json"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "stego-test-credential") || strings.Contains(string(raw), "-race") {
			t.Fatal("the record holds a value from the caller's environment")
		}
		var record appbuild.Record
		if err := json.Unmarshal(raw, &record); err != nil {
			t.Fatal(err)
		}
		cacheRecord := readCacheRecord(t, cache)
		if record.SourceRevision != revision || record.IndependentBuilds != 2 || record.DependencyProxy != "off" || record.ModuleCache == nil || *record.ModuleCache != cacheRecord.DownloadCache {
			t.Fatalf("the record does not name the revision, both builds and the module cache: %+v", record)
		}
		if record.GoVersion != appbuild.GoVersion || record.Toolchain.Files < 1 || len(record.Toolchain.SHA256) != 64 {
			t.Fatal("the record does not name the Go SDK")
		}
		if record.Artifact.SHA256 != fileSHA256(t, filepath.Join(results[0], "application")) {
			t.Fatal("the record does not name the executable's digest")
		}
		if record.GenerationStateSHA256 != fileSHA256(t, filepath.Join(source, ".stego", "state.yaml")) {
			t.Fatal("the record does not name the generation record")
		}
		tracked := strings.Split(git(t, source, "ls-files"), "\n")
		if len(record.Inputs) != len(tracked) {
			t.Fatalf("the record names %d inputs; the revision holds %d files", len(record.Inputs), len(tracked))
		}
		for _, input := range record.Inputs {
			if input.SHA256 != fileSHA256(t, filepath.Join(source, filepath.FromSlash(input.Path))) {
				t.Errorf("the record's digest of %s differs from the file", input.Path)
			}
		}
		for key, value := range map[string]string{"CGO_ENABLED": "0", "GOWORK": "off", "GOTOOLCHAIN": "local", "GOFLAGS": "", "GOPROXY": "off"} {
			if got, ok := record.Environment[key]; !ok || got != value {
				t.Errorf("the record sets %s to %q, expected %q", key, got, value)
			}
		}
		for _, key := range []string{"PATH", "HOME", "GH_TOKEN"} {
			if _, ok := record.Environment[key]; ok {
				t.Errorf("the record holds %s from a complete environment", key)
			}
		}
		flags := strings.Join(record.BuildFlags, " ")
		for _, flag := range []string{"-mod=readonly", "-trimpath", "-buildvcs=false", "-pgo=off"} {
			if !strings.Contains(flags, flag) {
				t.Errorf("the record's build settings lack %s", flag)
			}
		}
		if record.GoTelemetry != "off" {
			t.Error("the record does not name telemetry off")
		}
	})
	record := filepath.Join(results[0], "build.json")
	application := filepath.Join(results[0], "application")
	trusted := fileSHA256(t, record)
	t.Run("ST-BLD-UC04-R01-T01 A record whose canonical bytes do not hash to the trusted digest is refused", func(t *testing.T) {
		mustRun(t, runBuild, "verify", "--record", record, "--record-sha256", trusted, "--artifact", application)
		wrong := strings.Repeat("0", 64)
		_, err := capture(t, runBuild, "verify", "--record", record, "--record-sha256", wrong, "--artifact", application)
		if err == nil || !strings.Contains(err.Error(), "expected digest "+wrong) || !strings.Contains(err.Error(), "found "+trusted) {
			t.Fatalf("an untrusted digest was not refused with the expected and found digests: %v", err)
		}
		raw, err := os.ReadFile(record)
		if err != nil {
			t.Fatal(err)
		}
		duplicate := strings.Replace(string(raw), "  \"format\": 1,", "  \"format\": 1,\n  \"format\": 1,", 1)
		copied := filepath.Join(t.TempDir(), "build.json")
		writeProjectFile(t, copied, duplicate)
		if _, err := capture(t, runBuild, "verify", "--record", copied, "--record-sha256", fileSHA256(t, copied), "--artifact", application); err == nil || !strings.Contains(err.Error(), "canonical") {
			t.Fatalf("a record that is not canonical was accepted: %v", err)
		}
	})
	snapshot := filepath.Join(t.TempDir(), "snapshot")
	if err := os.Mkdir(snapshot, 0755); err != nil {
		t.Fatal(err)
	}
	archive := exec.Command("sh", "-c", "git -C \"$1\" archive --format=tar \"$2\" | tar -x -C \"$3\"", "sh", source, revision, snapshot)
	if output, err := archive.CombinedOutput(); err != nil {
		t.Fatalf("source snapshot: %v\n%s", err, output)
	}
	t.Run("ST-BLD-UC04-R01-T02 A source check refuses an untrusted record digest", func(t *testing.T) {
		mustRun(t, runBuild, "verify-source", "--record", record, "--record-sha256", trusted, "--source", snapshot)
		wrong := strings.Repeat("1", 64)
		if _, err := capture(t, runBuild, "verify-source", "--record", record, "--record-sha256", wrong, "--source", snapshot); err == nil || !strings.Contains(err.Error(), "expected digest "+wrong) {
			t.Fatalf("an untrusted digest was accepted: %v", err)
		}
	})
	t.Run("ST-BLD-UC04-R02-T01 A source snapshot that differs is refused, and each changed, missing or added file is named", func(t *testing.T) {
		changed := filepath.Join(t.TempDir(), "snapshot")
		copyTree := exec.Command("cp", "-a", snapshot, changed)
		if output, err := copyTree.CombinedOutput(); err != nil {
			t.Fatalf("copy: %v\n%s", err, output)
		}
		writeProjectFile(t, filepath.Join(changed, "go.sum"), "changed\n")
		if err := os.Remove(filepath.Join(changed, "service.yaml")); err != nil {
			t.Fatal(err)
		}
		writeProjectFile(t, filepath.Join(changed, "extra.go"), "package extra\n")
		_, err := capture(t, runBuild, "verify-source", "--record", record, "--record-sha256", trusted, "--source", changed)
		if err == nil {
			t.Fatal("a changed snapshot was accepted")
		}
		for _, want := range []string{"go.sum: changed", "service.yaml: missing", "extra.go: added"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the check does not name %q: %v", want, err)
			}
		}
	})
	t.Run("ST-BLD-UC04-R02-T02 An executable that differs is refused, and its recorded and found digests are named", func(t *testing.T) {
		raw, err := os.ReadFile(application)
		if err != nil {
			t.Fatal(err)
		}
		raw[len(raw)-1] ^= 0xff
		changed := filepath.Join(t.TempDir(), "application")
		if err := os.WriteFile(changed, raw, 0500); err != nil {
			t.Fatal(err)
		}
		var recorded appbuild.Record
		data, _ := os.ReadFile(record)
		if err := json.Unmarshal(data, &recorded); err != nil {
			t.Fatal(err)
		}
		_, err = capture(t, runBuild, "verify", "--record", record, "--record-sha256", trusted, "--artifact", changed)
		if err == nil || !strings.Contains(err.Error(), "executable digest: recorded "+recorded.Artifact.SHA256+", found "+fileSHA256(t, changed)) {
			t.Fatalf("a changed executable was not refused with both digests: %v", err)
		}
	})
	t.Run("ST-BLD-UC04-R03-T01 A record or an executable that cannot be read fails the check, is named, and is not changed", func(t *testing.T) {
		before := map[string]string{record: fileSHA256(t, record), application: fileSHA256(t, application)}
		missing := filepath.Join(t.TempDir(), "missing.json")
		folder := t.TempDir()
		for _, args := range [][]string{
			{"verify", "--record", missing, "--record-sha256", trusted, "--artifact", application},
			{"verify", "--record", folder, "--record-sha256", trusted, "--artifact", application},
			{"verify", "--record", record, "--record-sha256", trusted, "--artifact", missing},
			{"verify", "--record", record, "--record-sha256", trusted, "--artifact", folder},
			{"verify-source", "--record", missing, "--record-sha256", trusted, "--source", snapshot},
			{"verify-source", "--record", record, "--record-sha256", trusted, "--source", missing},
		} {
			_, err := capture(t, runBuild, args...)
			named := false
			for _, name := range []string{missing, folder} {
				if err != nil && strings.Contains(err.Error(), name) && strings.Contains(err.Error(), "cannot read") {
					named = true
				}
			}
			if !named {
				t.Errorf("stego build %v did not fail naming the input it cannot read: %v", args, err)
			}
		}
		for name, digest := range before {
			if fileSHA256(t, name) != digest {
				t.Errorf("a failed check changed %s", name)
			}
		}
	})
}

func TestBuildNeedsARecordedModuleCache(t *testing.T) {
	t.Run("ST-BLD-UC03-R01-T01 A build without a module cache is refused before any work, and names --module-cache", func(t *testing.T) {
		root := t.TempDir()
		source := filepath.Join(root, "source")
		if err := os.Mkdir(source, 0755); err != nil {
			t.Fatal(err)
		}
		args := []string{"--source", source, "--revision", strings.Repeat("a", 40), "--target", "out", "--go", pinnedGo(t), "--work", filepath.Join(root, "work"), "--output", filepath.Join(root, "result")}
		_, err := capture(t, runBuild, args...)
		if err == nil || !strings.Contains(err.Error(), "--module-cache") {
			t.Fatalf("a build without a module cache was not refused naming --module-cache: %v", err)
		}
		if names := folderNames(t, root); len(names) != 1 {
			t.Fatalf("a refused build made %v", names)
		}
	})
}

func TestBuildRecordThroughTheCommand(t *testing.T) {
	t.Run("ST-BLD-UC03-R03-T01 stego build verify refuses a record of one build, given its own digest", func(t *testing.T) {
		app := builtApplication(t)
		executable := filepath.Join(app.result, "application")
		mustStego(t, "build", "verify", "--record", filepath.Join(app.result, "build.json"), "--record-sha256", app.recordSHA256, "--artifact", executable)
		data, err := os.ReadFile(filepath.Join(app.result, "build.json"))
		if err != nil {
			t.Fatal(err)
		}
		var record appbuild.Record
		if err := json.Unmarshal(data, &record); err != nil {
			t.Fatal(err)
		}
		record.IndependentBuilds = 1
		changed, err := json.MarshalIndent(record, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		name := writeFile(t, t.TempDir(), "build.json", string(changed)+"\n", 0600)
		_, stderr, err := runStego(t, "build", "verify", "--record", name, "--record-sha256", fileSHA256(t, name), "--artifact", executable)
		if err == nil || !strings.Contains(stderr, "violates the build policy") {
			t.Fatalf("stego build verify of a record of one build: %v, %q; expected a refusal by the build policy", err, stderr)
		}
	})
}
