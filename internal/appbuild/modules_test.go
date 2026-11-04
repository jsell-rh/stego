package appbuild

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
)

func TestCompiledModulesRequireRecordedContent(t *testing.T) {
	t.Run("An executable whose compiled modules differ from the record is refused", func(t *testing.T) {
		sum := "h1:" + strings.Repeat("A", 43) + "="
		for _, name := range []string{"matching", "missing checksum", "wrong version", "wrong main", "extra module", "wrong checksum", "missing dependency"} {
			t.Run(name, func(t *testing.T) {
				record := fixtureRecord()
				record.Modules = append(record.Modules, Module{Path: "example.test/dep", Version: "v1.2.3", Sum: sum, GoModSum: sum})
				info := &debug.BuildInfo{Path: "example.test/app/out", Main: debug.Module{Path: "example.test/app", Version: "(devel)"}, Deps: []*debug.Module{{Path: "example.test/dep", Version: "v1.2.3", Sum: sum}}}
				switch name {
				case "missing checksum":
					record.Modules[1].Sum = ""
				case "wrong version":
					info.Deps[0].Version = "v1.2.4"
				case "wrong main":
					info.Main.Path = "example.test/other"
				case "extra module":
					record.Modules = append(record.Modules, Module{Path: "example.test/extra", Version: "v1.0.0", Sum: sum, GoModSum: sum})
				case "wrong checksum":
					info.Deps[0].Sum = "h1:" + strings.Repeat("B", 42) + "A="
				case "missing dependency":
					record.Modules = record.Modules[:1]
				}
				err := matchCompiledModules(info, &record)
				if (err == nil) != (name == "matching") {
					t.Fatal("unexpected module match", err)
				}
			})
		}
	})
}

func TestCompiledModulesRetainLocalAndRemoteReplacementIdentity(t *testing.T) {
	sum := "h1:" + strings.Repeat("A", 43) + "="
	for _, local := range []bool{false, true} {
		record := fixtureRecord()
		replacement := Module{Path: "example.test/fork", Version: "v2.0.0", Sum: sum, GoModSum: sum}
		actual := &debug.Module{Path: replacement.Path, Version: replacement.Version, Sum: sum}
		if local {
			replacement = Module{Path: "../common", LocalPath: "common"}
			actual = &debug.Module{Path: "../common", Version: "(devel)"}
		}
		record.Modules = append(record.Modules, Module{Path: "example.test/original", Version: "v1.0.0", Replacement: &replacement})
		info := &debug.BuildInfo{Path: "example.test/app/out", Main: debug.Module{Path: "example.test/app", Version: "(devel)"}, Deps: []*debug.Module{{Path: "example.test/original", Version: "v1.0.0", Replace: actual}}}
		if err := matchCompiledModules(info, &record); err != nil {
			t.Fatal(err)
		}
		info.Deps[0].Replace.Path = "another-replacement"
		if err := matchCompiledModules(info, &record); err == nil {
			t.Fatal("changed replacement accepted")
		}
	}
}

func TestSourceChangeRecordRetainsBeforeAndAfterDigests(t *testing.T) {
	root := t.TempDir()
	before := []File{{"go.sum", strings.Repeat("a", 64), false}, {"removed", strings.Repeat("b", 64), false}}
	after := []File{{"go.sum", strings.Repeat("c", 64), false}, {"added", strings.Repeat("d", 64), false}}
	if err := saveSourceChange(root, before, after); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "source-change.json"))
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]map[string]File
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if len(record) != 3 || record["go.sum"]["before"] != before[0] || record["go.sum"]["after"] != after[0] || record["removed"]["before"] != before[1] || record["added"]["after"] != after[1] {
		t.Fatal("source change record differs", record)
	}
}

func TestCanceledBuildDoesNotInspectOrWriteInputs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Build(ctx, Options{}); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation was lost", err)
	}
}

func TestExecutableMismatchesNameEachDifference(t *testing.T) {
	t.Run("ST-BLD-DS02-T01 An executable whose digest, size, build settings or modules differ from the record is refused, and each is named", func(t *testing.T) {
		record := fixtureRecord()
		record.BinarySettings = map[string]string{"-trimpath": "true", "CGO_ENABLED": "0"}
		info := &debug.BuildInfo{Path: "example.test/app/out", Main: debug.Module{Path: "example.test/app"}}
		if mismatches := executableMismatches(&record, record.Artifact, record.BinarySettings, info); len(mismatches) != 0 {
			t.Fatalf("a matching executable was refused: %v", mismatches)
		}
		found := Artifact{strings.Repeat("0", 64), 99}
		settings := map[string]string{"-trimpath": "false", "-race": "true"}
		info.Deps = []*debug.Module{{Path: "example.test/extra", Version: "v1.0.0"}}
		mismatches := strings.Join(executableMismatches(&record, found, settings, info), "\n")
		for _, want := range []string{
			"executable digest: recorded " + record.Artifact.SHA256 + ", found " + found.SHA256,
			"executable size: recorded 100, found 99",
			`build setting -trimpath: recorded "true", found "false"`,
			`build setting -race: recorded none, found "true"`,
			`build setting CGO_ENABLED: recorded "0", found none`,
			"module example.test/extra v1.0.0: compiled, but not in the record",
		} {
			if !strings.Contains(mismatches, want) {
				t.Errorf("the check does not name %q:\n%s", want, mismatches)
			}
		}
	})
}
