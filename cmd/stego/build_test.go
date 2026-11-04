package main

import (
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
