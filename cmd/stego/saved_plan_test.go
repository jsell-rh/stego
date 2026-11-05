package main

import (
	"maps"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// These tests start from ST-GEN-TD03, save a plan with stego plan and carry it
// out with stego apply --saved-plan, through the binary.

type savedPlanFile struct {
	Path   string `yaml:"path"`
	Action string `yaml:"action"`
	SHA256 string `yaml:"sha256"`
}

// savedPlan reads .stego/plan.yaml with a YAML parser and no STEGO code.
func (s *smallService) savedPlan(t *testing.T) (files []savedPlanFile, recordChanges bool) {
	t.Helper()
	var plan struct {
		RecordSHA256 string          `yaml:"record_sha256"`
		Files        []savedPlanFile `yaml:"files"`
	}
	if err := yaml.Unmarshal([]byte(s.read(t, ".stego/plan.yaml")), &plan); err != nil {
		t.Fatal(err)
	}
	return plan.Files, plan.RecordSHA256 != ""
}

// changedOutput gives each path under out whose hash differs between the two
// trees, with its new hash, or "" when it is gone.
func changedOutput(before, after map[string]string) map[string]string {
	changed := map[string]string{}
	for path, hash := range after {
		if strings.HasPrefix(path, "out/") && before[path] != hash {
			changed[strings.TrimPrefix(path, "out/")] = hash
		}
	}
	for path := range before {
		if _, kept := after[path]; !kept && strings.HasPrefix(path, "out/") {
			changed[strings.TrimPrefix(path, "out/")] = ""
		}
	}
	return changed
}

// plannedOutput gives each path under out that the saved plan names, with the
// hash it plans, or "" for a delete.
func plannedOutput(files []savedPlanFile) map[string]string {
	planned := map[string]string{}
	for _, file := range files {
		if file.Path == "go.mod" || file.Path == "go.sum" {
			continue
		}
		planned[file.Path] = file.SHA256
	}
	return planned
}

func TestApplyCarriesOutSavedPlan(t *testing.T) {
	t.Run("ST-GEN-UC05-R03-T01 Apply of a saved plan writes exactly the files that the plan names, with the planned content", func(t *testing.T) {
		s := newSmallService(t)
		s.write(t, "service.yaml", tagDeclaration)
		s.must(t, "plan")
		files, _ := s.savedPlan(t)
		planned := plannedOutput(files)
		actions := map[string]bool{}
		for _, file := range files {
			actions[file.Action] = true
		}
		if !actions["generate"] || !actions["update"] || !actions["delete"] {
			t.Fatalf("the saved plan does not create, update and delete files: %v", files)
		}
		before := tree(t, s.service)
		s.must(t, "apply", "--saved-plan")
		if changed := changedOutput(before, tree(t, s.service)); !maps.Equal(changed, planned) {
			t.Fatalf("apply changed %v under out, and the saved plan names %v", changed, planned)
		}
	})

	t.Run("ST-GEN-UC05-R03-T02 Apply refuses a saved plan whose inputs changed after planning, and writes nothing", func(t *testing.T) {
		s := newSmallService(t)
		s.write(t, "service.yaml", secondEntityDeclaration)
		s.must(t, "plan")
		s.write(t, "service.yaml", strings.Replace(secondEntityDeclaration, "{name: label, type: string}", "{name: label, type: string}, {name: color, type: string}", 1))
		before := tree(t, s.service)
		stdout, stderr, err := s.stego(t, "apply", "--saved-plan")
		if err == nil || !strings.Contains(stdout+stderr, "stale") {
			t.Fatalf("apply of a stale saved plan did not refuse and say that it is stale: %v\n%s%s", err, stdout, stderr)
		}
		if !maps.Equal(before, tree(t, s.service)) {
			t.Fatal("a refused apply of a stale saved plan changed a file")
		}
	})
}

func TestSavedPlanFlow(t *testing.T) {
	t.Run("ST-GEN-UC05-FL-T A saved plan lists every change, and apply then makes exactly those changes", func(t *testing.T) {
		s := newSmallService(t)
		s.write(t, "service.yaml", tagDeclaration)
		before := tree(t, s.service)
		printed := s.must(t, "plan")
		after := tree(t, s.service)
		for path, hash := range after {
			if before[path] != hash && !strings.HasPrefix(path, ".stego/") {
				t.Errorf("plan changed %s outside .stego", path)
			}
		}
		files, recordChanges := s.savedPlan(t)
		if !recordChanges || !strings.Contains(printed, "compiler state") {
			t.Fatalf("the plan does not say that the record changes:\n%s", printed)
		}
		for _, file := range files {
			if !strings.Contains(printed, file.Path) {
				t.Errorf("the printed plan does not list %s", file.Path)
			}
		}
		shown := s.must(t, "plan", "--show")
		if !strings.Contains(shown, "still applies") {
			t.Fatalf("the saved plan does not show that it still applies:\n%s", shown)
		}
		before = tree(t, s.service)
		s.must(t, "apply", "--saved-plan")
		if changed := changedOutput(before, tree(t, s.service)); !maps.Equal(changed, plannedOutput(files)) {
			t.Fatalf("apply changed %v under out, and the saved plan names %v", changed, plannedOutput(files))
		}

		s.write(t, "service.yaml", smallServiceDeclaration)
		s.must(t, "plan")
		s.write(t, "service.yaml", secondEntityDeclaration)
		before = tree(t, s.service)
		stdout, stderr, err := s.stego(t, "apply", "--saved-plan")
		if err == nil || !strings.Contains(stdout+stderr, "stale") {
			t.Fatalf("apply of a stale saved plan did not refuse and say that it is stale: %v\n%s%s", err, stdout, stderr)
		}
		if !maps.Equal(before, tree(t, s.service)) {
			t.Fatal("a refused apply changed a file")
		}
	})
}
