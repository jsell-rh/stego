package compiler

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/parser"
	"gopkg.in/yaml.v3"
)

type manifestInputGenerator struct{}

func (manifestInputGenerator) InputFiles(map[string]any) ([]gen.InputFile, error) {
	return gen.SourceInputs([]string{"api/input.txt"}), nil
}
func (manifestInputGenerator) Generate(gen.Context) ([]gen.File, *gen.Wiring, error) {
	return []gen.File{{Path: "internal/api/handler.go", Content: []byte("package api\n")}}, nil, nil
}

func TestInputManifestRecordsChangedInputsWithoutOutputChanges(t *testing.T) {
	input := snapshotTestInput(t)
	input.Generators["stub-api"] = manifestInputGenerator{}
	os.Mkdir(filepath.Join(input.ProjectDir, "api"), 0755)
	source := filepath.Join(input.ProjectDir, "api/input.txt")
	writeFile(t, source, "first input")
	applyInitialSnapshot(t, input)
	plan, err := Reconcile(input)
	if err != nil {
		t.Fatal(err)
	}
	// Creating go.mod changes a future input. Record it before the stable check.
	if err := Apply(plan, input.ProjectDir, ""); err != nil {
		t.Fatal(err)
	}
	stable, err := Reconcile(input)
	if err != nil || stable.HasChanges() {
		t.Fatal("identical inputs changed the plan", err)
	}
	before := stable.NewState.LastApplied.Inputs
	if before.Files["api/input.txt"].SHA256 != HashBytes([]byte("first input")) || before.Files["go.sum"].Exists || before.Options.ModuleName != input.ModuleName {
		t.Fatal("incomplete manifest", before)
	}
	if _, ok := before.Files[".stego/state.yaml"]; ok {
		t.Fatal("manifest includes itself")
	}
	writeFile(t, source, "second input")
	plan, err = Reconcile(input)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.StateChanged || plan.hasOutputChanges() || plan.NewState.LastApplied.Inputs.SHA256 == before.SHA256 {
		t.Fatal("input change was hidden")
	}
	if err := Apply(plan, input.ProjectDir, ""); err != nil {
		t.Fatal(err)
	}
	state, err := LoadState(filepath.Join(input.ProjectDir, ".stego/state.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if state.LastApplied.Inputs.Files["api/input.txt"].SHA256 != HashBytes([]byte("second input")) {
		t.Fatal("wrong input saved")
	}
	input.RegistrySHA = "new-label"
	plan, err = Reconcile(input)
	if err != nil || !plan.StateChanged || plan.NewState.LastApplied.Inputs.Options.RegistryRef != "new-label" {
		t.Fatal("option change was hidden", err)
	}
}

func TestInputManifestRejectsPlanSubstitution(t *testing.T) {
	input := snapshotTestInput(t)
	applyInitialSnapshot(t, input)
	plan, err := Reconcile(input)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(input.ProjectDir, ".stego/state.yaml")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	manifest := plan.NewState.LastApplied.Inputs
	manifest.Options.GoVersion = "1.1"
	manifest.SHA256 = manifest.digest()
	if err := Apply(plan, input.ProjectDir, ""); err == nil || !strings.Contains(err.Error(), "manifest") {
		t.Fatal("substituted manifest accepted", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("failed apply changed state", err)
	}
	state, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	state.LastApplied.Inputs = nil
	if err := SaveState(path, state); err != nil {
		t.Fatal(err)
	}
	plan, err = Reconcile(input)
	if err != nil || !plan.StateChanged || plan.NewState.LastApplied.Inputs == nil {
		t.Fatal("old state did not acquire manifest", err)
	}
}

func TestManifestUsesCapturedModuleBytes(t *testing.T) {
	input := snapshotTestInput(t)
	original := []byte("module example.com/snapshot\ngo 1.26.11\nrequire example.com/original v1.0.0\n")
	path := filepath.Join(input.ProjectDir, "go.mod")
	writeFile(t, path, string(original))
	service, err := os.ReadFile(filepath.Join(input.ProjectDir, "service.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	_, snapshots, data, err := captureProjectInputs(input.ProjectDir, service)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, "module example.com/snapshot\ngo 1.26.11\nrequire example.com/intermediate v1.0.0\n")
	result, err := mergeCapturedModule(data, snapshots["go.mod"].Exists, gen.File{Path: "go.mod", Content: []byte("module example.com/snapshot\ngo 1.26.11\n")})
	if err != nil || !bytes.Equal(result.Content, original) {
		t.Fatal("module merge read later filesystem data", err)
	}
	writeFile(t, path, string(original))
	manifest, err := newInputManifest(input, "out", false, snapshots)
	if err != nil || manifest.Files["go.mod"].SHA256 != HashBytes(result.Content) {
		t.Fatal("record does not identify consumed module", err)
	}
}

func manifestFixture() *InputManifest {
	m := &InputManifest{Version: 1, Options: InputOptions{"example.com/demo", "1.26.8", "out", "local", nil}, Files: map[string]InputFile{
		"service.yaml": {Exists: true, SHA256: strings.Repeat("a", 64), Mode: 0644}, "go.mod": {}, "go.sum": {}, ".stego/config.yaml": {},
	}}
	m.SHA256 = m.digest()
	return m
}
func TestInputManifestRejectsMalformedState(t *testing.T) {
	for _, change := range []func(*InputManifest){
		func(m *InputManifest) { m.Version = 3 },
		func(m *InputManifest) {
			for i := len(m.Files); i <= maxManifestFiles; i++ {
				m.Files[fmt.Sprintf("extra/%d", i)] = m.Files["service.yaml"]
			}
		}, func(m *InputManifest) { m.SHA256 = strings.Repeat("0", 64) },
		func(m *InputManifest) { m.Files["service.yaml"] = InputFile{} }, func(m *InputManifest) { delete(m.Files, "go.sum") },
		func(m *InputManifest) { m.Files["../escape"] = m.Files["service.yaml"] }, func(m *InputManifest) { m.Files["out/file.go"] = m.Files["service.yaml"] },
		func(m *InputManifest) { m.Files[".stego/state.yaml"] = m.Files["service.yaml"] }, func(m *InputManifest) { m.Options.OutputDir = "../escape" },
		func(m *InputManifest) { m.Files["go.mod"] = InputFile{SHA256: strings.Repeat("a", 64)} },
		func(m *InputManifest) { f := m.Files["service.yaml"]; f.Mode = 01000; m.Files["service.yaml"] = f },
		func(m *InputManifest) {
			f := m.Files["service.yaml"]
			f.SHA256 = strings.Repeat("A", 64)
			m.Files["service.yaml"] = f
		},
	} {
		m := manifestFixture()
		original := m.SHA256
		change(m)
		if m.SHA256 == original {
			m.SHA256 = m.digest()
		}
		if err := m.validate(); err == nil {
			t.Fatal("malformed manifest with computed digest accepted", m)
		}
		data, err := yaml.Marshal(&State{LastApplied: &AppliedState{Inputs: m, ServiceHash: strings.Repeat("a", 64), RegistrySHA: "local"}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := decodeState(data, "fixture"); err == nil {
			t.Fatal("malformed manifest accepted", m)
		}
	}
}

func TestInputManifestDigestVector(t *testing.T) {
	m := manifestFixture()
	// This value was computed with an independent Python implementation.
	if m.SHA256 != "ba23895de3506ff6083120f019051ac0f48ec2000548646e14b0a127446521a1" {
		t.Fatal(m.SHA256)
	}
	input := ReconcilerInput{ProjectDir: "/unused/a", RegistryDir: "/unused/r", ModuleName: m.Options.ModuleName, GoVersion: m.Options.GoVersion, RegistrySHA: m.Options.RegistryRef}
	snapshots := map[string]fileSnapshot{}
	for name, file := range m.Files {
		snapshots[name] = fileSnapshot{Exists: file.Exists, Hash: file.SHA256, Mode: os.FileMode(file.Mode)}
	}
	// A new manifest is version 2, which also digests fill presence. This
	// value was computed with the same independent Python implementation.
	const version2 = "3af49acb30802346258f90f397bb36a63c9d90c14a73f3e242adb4562510be7a"
	moved, err := newInputManifest(input, "out", false, snapshots)
	if err != nil || moved.SHA256 != version2 {
		t.Fatal("location changed input identity", err)
	}
	input.ProjectDir = "/another/project"
	input.RegistryDir = "/another/registry"
	moved, err = newInputManifest(input, "out", false, snapshots)
	if err != nil || moved.SHA256 != version2 {
		t.Fatal("location changed input identity", err)
	}
}

func TestOversizedStateCannotBeSaved(t *testing.T) {
	input := snapshotTestInput(t)
	applyInitialSnapshot(t, input)
	plan, err := Reconcile(input)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(input.ProjectDir, ".stego/state.yaml")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	plan.NewState.LastApplied.Components["large"] = ComponentState{Version: strings.Repeat("v", parser.MaxDocumentBytes)}
	if err := SaveState(path, plan.NewState); err == nil || !strings.Contains(err.Error(), "state exceeds") {
		t.Fatal("oversized state saved", err)
	}
	if err := Apply(plan, input.ProjectDir, ""); err == nil || !strings.Contains(err.Error(), "state exceeds") {
		t.Fatal("oversized state committed", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("rejected state changed files", err)
	}
	if _, err := os.Stat(filepath.Join(input.ProjectDir, transactionPath)); !os.IsNotExist(err) {
		t.Fatal("rejected state started a transaction", err)
	}
}

func BenchmarkProjectInputManifest(b *testing.B) {
	for _, size := range []int{14, 1024} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			input := ReconcilerInput{ModuleName: "example.com/demo", GoVersion: "1.26.11", RegistrySHA: "local"}
			snapshots := map[string]fileSnapshot{}
			for _, name := range []string{"service.yaml", "go.mod", "go.sum", ".stego/config.yaml"} {
				snapshots[name] = fileSnapshot{Exists: true, Hash: strings.Repeat("a", 64), Mode: 0644}
			}
			for n := len(snapshots); n < size; n++ {
				snapshots[fmt.Sprintf("contracts/%04d.proto", n)] = fileSnapshot{Exists: true, Hash: strings.Repeat("b", 64), Mode: 0644}
			}
			b.ReportAllocs()
			for b.Loop() {
				m, err := newInputManifest(input, "out", false, snapshots)
				if err != nil {
					b.Fatal(err)
				}
				if err := m.validate(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestInputManifestMustMatchAppliedState(t *testing.T) {
	t.Run("ST-GEN-INV02-T A record whose input manifest disagrees with it is refused", func(t *testing.T) {
		m := manifestFixture()
		for _, state := range []*AppliedState{
			{Inputs: m, ServiceHash: strings.Repeat("b", 64), RegistrySHA: "local"},
			{Inputs: m, ServiceHash: strings.Repeat("a", 64), RegistrySHA: "other"},
		} {
			if err := validateStatePaths(&State{LastApplied: state}); err == nil {
				t.Fatal("contradictory input record accepted")
			}
		}
	})
}

func TestInputManifestIsBoundedAndCanonical(t *testing.T) {
	t.Run("ST-GEN-INV23-T The input manifest is bounded, canonical and digested one way", func(t *testing.T) {
		for name, change := range map[string]func(*InputManifest){
			"1025 files": func(m *InputManifest) {
				for i := len(m.Files); i <= maxManifestFiles; i++ {
					m.Files[fmt.Sprintf("contracts/%04d.proto", i)] = m.Files["service.yaml"]
				}
			},
			"an absolute path":           func(m *InputManifest) { m.Files["/etc/service.yaml"] = m.Files["service.yaml"] },
			"a path under the output":    func(m *InputManifest) { m.Files["out/handler.go"] = m.Files["service.yaml"] },
			"the private state as input": func(m *InputManifest) { m.Files[".stego/state.yaml"] = m.Files["service.yaml"] },
		} {
			m := manifestFixture()
			change(m)
			m.SHA256 = m.digest()
			path := filepath.Join(t.TempDir(), "absent", "state.yaml")
			err := SaveState(path, &State{LastApplied: &AppliedState{Inputs: m, ServiceHash: m.Files["service.yaml"].SHA256, RegistrySHA: m.Options.RegistryRef}})
			if err == nil {
				t.Fatalf("a manifest with %s was saved", name)
			}
			if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
				t.Fatalf("a refused manifest with %s wrote %s: %v", name, filepath.Dir(path), err)
			}
		}

		m := manifestFixture()
		path := filepath.Join(t.TempDir(), "absent", "state.yaml")
		state := &State{LastApplied: &AppliedState{Inputs: m, ServiceHash: m.Files["service.yaml"].SHA256, RegistrySHA: m.Options.RegistryRef,
			Components: map[string]ComponentState{"large": {Version: strings.Repeat("v", parser.MaxDocumentBytes)}}}}
		if err := SaveState(path, state); err == nil || !strings.Contains(err.Error(), "state exceeds") {
			t.Fatalf("a record over the 4 MiB limit was saved: %v", err)
		}
		if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
			t.Fatalf("a refused record over the limit wrote %s: %v", filepath.Dir(path), err)
		}

		// The digest of the fixed manifest, computed with an independent
		// Python implementation.
		if digest := manifestFixture().SHA256; digest != "ba23895de3506ff6083120f019051ac0f48ec2000548646e14b0a127446521a1" {
			t.Fatalf("the digest of the fixed manifest is %s, not its vector", digest)
		}
	})
}

func TestInputManifestFillPresence(t *testing.T) {
	t.Run("ST-GEN-INV23-T A version 2 manifest digests whether the project holds fills, and its fields must suit its version", func(t *testing.T) {
		with, without := manifestFixture(), manifestFixture()
		holds, none := true, false
		with.Version, with.Options.HoldsFills = 2, &holds
		without.Version, without.Options.HoldsFills = 2, &none
		with.SHA256, without.SHA256 = with.digest(), without.digest()
		if with.SHA256 == without.SHA256 || with.SHA256 == manifestFixture().SHA256 {
			t.Fatal("fill presence does not change the digest")
		}
		for _, m := range []*InputManifest{with, without, manifestFixture()} {
			if err := m.validate(); err != nil {
				t.Fatalf("valid manifest refused: %v", err)
			}
		}
		noRecord := manifestFixture()
		noRecord.Version = 2
		noRecord.SHA256 = noRecord.digest()
		oldWithRecord := manifestFixture()
		oldWithRecord.Options.HoldsFills = &holds
		oldWithRecord.SHA256 = oldWithRecord.digest()
		for _, m := range []*InputManifest{noRecord, oldWithRecord} {
			if err := m.validate(); err == nil {
				t.Fatalf("manifest version %d with holds_fills %v accepted", m.Version, m.Options.HoldsFills)
			}
		}
	})
}
