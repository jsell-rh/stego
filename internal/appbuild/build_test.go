package appbuild

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jsell-rh/stego/internal/buildidentity"
)

func put(t *testing.T, root, name, value string) {
	t.Helper()
	full := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestInventoryTracksFilesAndRejectsLinks(t *testing.T) {
	root := t.TempDir()
	put(t, root, "b", "second")
	put(t, root, "a", "first")
	first, files, err := inventory(root, 2, 11)
	if err != nil || first.Files != 2 || first.Bytes != 11 || files[0].Path != "a" {
		t.Fatal(first, files, err)
	}
	if err := os.Chmod(filepath.Join(root, "a"), 0700); err != nil {
		t.Fatal(err)
	}
	second, _, err := inventory(root, 2, 11)
	if err != nil || first.SHA256 == second.SHA256 {
		t.Fatal("executable change was not recorded", err)
	}
	if _, _, err = inventory(root, 1, 11); err == nil {
		t.Fatal("file limit was ignored")
	}
	if _, _, err = inventory(root, 2, 10); err == nil {
		t.Fatal("byte limit was ignored")
	}
	if err := os.Symlink(filepath.Join(root, "a"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if _, _, err = inventory(root, 3, 100); err == nil {
		t.Fatal("link was accepted")
	}
}

func TestInventoryRetainsOfficialSDKASCIIEncoding(t *testing.T) {
	data, err := inventoryJSON([][]any{{"Þfoo.go", "<&>", false}, {"😀.go", "hash", true}})
	want := `[["\u00defoo.go","<&>",false],["\ud83d\ude00.go","hash",true]]`
	if err != nil || string(data) != want {
		t.Fatalf("inventory encoding differs: %s; %v", data, err)
	}
	root := t.TempDir()
	put(t, root, "Þfoo.go", "hello")
	result, _, err := inventory(root, 1, 5)
	if err != nil || result.SHA256 != "c54115d622b96c6dd2b897ffc44eb9a74c77c79ee11cc293381efd9f329d873f" {
		t.Fatal("inventory differs from the existing compiler format", result, err)
	}
}

func TestArchiveRejectsUnsafeEntries(t *testing.T) {
	for _, header := range []*tar.Header{
		{Name: "../escape", Typeflag: tar.TypeReg},
		{Name: "/absolute", Typeflag: tar.TypeReg},
		{Name: "a/../escape", Typeflag: tar.TypeReg},
		{Name: "a\\b", Typeflag: tar.TypeReg},
		{Name: ".git/config", Typeflag: tar.TypeReg},
		{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/outside"},
		{Name: "hard", Typeflag: tar.TypeLink, Linkname: "other"},
		{Name: "fifo", Typeflag: tar.TypeFifo},
	} {
		t.Run(header.Name, func(t *testing.T) {
			var data bytes.Buffer
			writer := tar.NewWriter(&data)
			if err := writer.WriteHeader(header); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if err := unpack(&data, t.TempDir()); err == nil {
				t.Fatal("unsafe entry was accepted")
			}
		})
	}
	var data bytes.Buffer
	writer := tar.NewWriter(&data)
	for i := 0; i < 2; i++ {
		if err := writer.WriteHeader(&tar.Header{Name: "duplicate", Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := unpack(&data, t.TempDir()); err == nil {
		t.Fatal("duplicate was accepted")
	}
}

func TestArchiveCopiesOnlyDeclaredBytes(t *testing.T) {
	var data bytes.Buffer
	writer := tar.NewWriter(&data)
	if err := writer.WriteHeader(&tar.Header{Name: "nested/file", Typeflag: tar.TypeReg, Size: 5, Mode: 0755}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := unpack(&data, root); err != nil {
		t.Fatal(err)
	}
	_, files, err := inventory(root, 1, 5)
	if err != nil || len(files) != 1 || !files[0].Executable || files[0].SHA256 != digest([]byte("hello")) {
		t.Fatal(files, err)
	}
}

func TestLocalReplacementCannotEscapeSource(t *testing.T) {
	root := t.TempDir()
	for _, replacement := range []string{"../../outside", "/etc", "../bad:volume", "../bad\\path"} {
		put(t, root, "app/go.mod", "module example.test/app\ngo 1.26.8\nreplace example.test/common => "+replacement+"\n")
		if err := checkModules(root, filepath.Join(root, "app"), map[string]bool{}); err == nil {
			t.Fatal("accepted replacement", replacement)
		}
	}
	put(t, root, "app/go.mod", "module example.test/app\ngo 1.26.8\nreplace example.test/common => ../common\n")
	put(t, root, "common/go.mod", "module example.test/common\ngo 1.25.0\n")
	if err := checkModules(root, filepath.Join(root, "app"), map[string]bool{}); err != nil {
		t.Fatal(err)
	}
}

func TestModuleInventoryRecordsReplacementsWithoutHostPaths(t *testing.T) {
	root := t.TempDir()
	sum := "h1:" + strings.Repeat("A", 43) + "="
	values := []goModule{
		{Path: "example.test/app", Main: true, Dir: filepath.Join(root, "app")},
		{Path: "example.test/common", Version: "v1.0.0", Replace: &goModule{Path: "../common", Dir: filepath.Join(root, "common")}},
		{Path: "example.test/remote", Version: "v1.0.0", Sum: sum, GoModSum: sum},
	}
	var data bytes.Buffer
	for _, value := range values {
		if err := json.NewEncoder(&data).Encode(value); err != nil {
			t.Fatal(err)
		}
	}
	modules, err := moduleInventory(data.Bytes(), root)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(modules)
	if bytes.Contains(encoded, []byte(root)) || modules[1].Replacement.LocalPath != "common" {
		t.Fatal("host path leaked", modules)
	}
	if _, err := moduleInventory(append(data.Bytes(), data.Bytes()...), root); err == nil {
		t.Fatal("repeated modules accepted")
	}
	values[2].Sum = "h1:wrong"
	raw, _ := json.Marshal(values[2])
	if _, err := moduleInventory(raw, root); err == nil {
		t.Fatal("bad checksum accepted")
	}
}

func TestEnvironmentExcludesAmbientBuildAndCredentialSettings(t *testing.T) {
	for _, key := range []string{"GOFLAGS", "GOWORK", "GOENV", "GOCACHEPROG", "LD_PRELOAD", "GH_TOKEN", "HTTPS_PROXY"} {
		t.Setenv(key, "unexpected-private-value")
	}
	env := environment("/work", "/sdk", true)
	for _, value := range env {
		if strings.Contains(value, "unexpected-private-value") {
			t.Fatal("ambient setting was inherited")
		}
	}
	if !strings.Contains(strings.Join(env, "\n"), "GOPROXY=off\n") {
		t.Fatal("build can fetch dependencies")
	}
}

func fixtureRecord() Record {
	compiler := buildidentity.Record{GoVersion: GoVersion, GOOS: "linux", GOARCH: "amd64", ModuleVersion: "(devel)", VCS: "git", Revision: strings.Repeat("a", 40), SourceState: "clean"}
	inputs := []File{{".stego/state.yaml", strings.Repeat("b", 64), false}, {"out/main.go", strings.Repeat("c", 64), false}}
	data, _ := json.Marshal([][]any{{inputs[0].Path, inputs[0].SHA256, false}, {inputs[1].Path, inputs[1].SHA256, false}})
	return Record{Format: 1, SourceRevision: strings.Repeat("a", 40), Source: Inventory{digest(data), 2, 100}, Inputs: inputs, Module: ".", Target: "out", GenerationStateSHA256: strings.Repeat("b", 64), GenerationCompiler: compiler, BuildCompiler: compiler, BuildCompilerArtifact: Artifact{strings.Repeat("d", 64), 100}, Toolchain: sdkIdentity, GoVersion: GoVersion, GitSHA256: strings.Repeat("e", 64), Environment: buildEnvironmentRecord(), DependencyProxy: "https://proxy.golang.org", GoTelemetry: "off", BuildFlags: []string{"build", "-mod=readonly", "-trimpath", "-buildvcs=false", "-pgo=off", "-p=2", "-o", "<artifact>", "./out"}, Modules: []Module{{Path: "example.test/app", LocalPath: "."}}, Artifact: Artifact{strings.Repeat("f", 64), 100}, IndependentBuilds: 2}
}

func TestRecordRejectsChangedBuildPolicyAndInputs(t *testing.T) {
	valid := fixtureRecord()
	if err := validateRecord(&valid); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Record){
		"one build":       func(r *Record) { r.IndependentBuilds = 1 },
		"toolchain":       func(r *Record) { r.Toolchain.SHA256 = strings.Repeat("0", 64) },
		"ambient flags":   func(r *Record) { r.Environment["GOFLAGS"] = "-race" },
		"target escape":   func(r *Record) { r.Target = "../outside" },
		"file change":     func(r *Record) { r.Inputs[0].SHA256 = strings.Repeat("0", 64) },
		"file order":      func(r *Record) { r.Inputs[0], r.Inputs[1] = r.Inputs[1], r.Inputs[0] },
		"missing modules": func(r *Record) { r.Modules = nil },
		"bad replacement": func(r *Record) { r.Modules[0].LocalPath = "../outside" },
		"build flags":     func(r *Record) { r.BuildFlags = append(r.BuildFlags, "-race") },
	} {
		t.Run(name, func(t *testing.T) {
			record := fixtureRecord()
			change(&record)
			if err := validateRecord(&record); err == nil {
				t.Fatal("changed record accepted")
			}
		})
	}
}

func TestRecordVerificationRequiresTrustedCanonicalBytes(t *testing.T) {
	r := fixtureRecord()
	data, _ := json.MarshalIndent(r, "", "  ")
	data = append(data, '\n')
	root := t.TempDir()
	name := filepath.Join(root, "build.json")
	if err := os.WriteFile(name, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(name, "missing", strings.Repeat("0", 64)); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatal("changed record digest accepted", err)
	}
	duplicate := bytes.Replace(data, []byte("  \"format\": 1,"), []byte("  \"format\": 1,\n  \"format\": 1,"), 1)
	if err := os.WriteFile(name, duplicate, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(name, "missing", digest(duplicate)); err == nil || !strings.Contains(err.Error(), "canonical") {
		t.Fatal("duplicate field accepted", err)
	}
}

func TestBuildPathValidationProtectsExistingDirectories(t *testing.T) {
	root := t.TempDir()
	put(t, root, "sdk/bin/go", "not executed")
	if err := os.Mkdir(filepath.Join(root, "source"), 0700); err != nil {
		t.Fatal(err)
	}
	base := Options{Source: filepath.Join(root, "source"), Revision: strings.Repeat("a", 40), Module: ".", Target: "out", Go: filepath.Join(root, "sdk/bin/go"), Work: filepath.Join(root, "work"), Output: filepath.Join(root, "output")}
	if _, err := prepare(base); err != nil {
		t.Fatal(err)
	}
	bad := base
	bad.Output = base.Source
	if _, err := prepare(bad); err == nil {
		t.Fatal("existing output accepted")
	}
	bad = base
	bad.Work = filepath.Join(base.Source, "work")
	if _, err := prepare(bad); err == nil {
		t.Fatal("source overlap accepted")
	}
	if _, err := Build(context.Background(), base); err == nil {
		t.Fatal("unverified SDK accepted")
	}
	if _, err := os.Stat(base.Work); !os.IsNotExist(err) {
		t.Fatal("unverified SDK caused a work directory write", err)
	}
}

func TestCommandRejectsOutputLimitAndCancellation(t *testing.T) {
	var output bytes.Buffer
	if err := command(context.Background(), t.TempDir(), []string{"PATH=/usr/bin:/bin"}, &output, 4, "/bin/sh", "-c", "printf 12345678"); err == nil {
		t.Fatal("output limit ignored")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := command(ctx, t.TempDir(), []string{"PATH=/usr/bin:/bin"}, &output, 100, "/bin/sh", "-c", "sleep 10 & wait")
	if err == nil || time.Since(start) > 3*time.Second {
		t.Fatal("process group was not canceled", err)
	}
	if reflect.DeepEqual(environment("/one", "/sdk", true), environment("/two", "/sdk", true)) {
		t.Fatal("independent cache paths did not change")
	}
}
