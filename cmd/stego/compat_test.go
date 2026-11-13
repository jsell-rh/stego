package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// compatResult is stego compat --json as the test reads it, with no STEGO code.
type compatResult struct {
	BaseRef    string `json:"base_ref"`
	BaseCommit string `json:"base_commit"`
	Faults     []struct {
		Rule    string `json:"rule"`
		Entity  string `json:"entity"`
		Field   string `json:"field"`
		Message string `json:"message"`
		Declare []struct {
			ID    string `json:"id"`
			Kind  string `json:"kind"`
			Field string `json:"field"`
			From  string `json:"from"`
			Needs string `json:"needs"`
			Phase string `json:"phase"`
		} `json:"declare"`
	} `json:"faults"`
}

// exitCode gives the exit status of a finished stego run.
func exitCode(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("stego did not run: %v", err)
	}
	return exit.ExitCode()
}

// compat runs stego compat in the service folder and gives its output and exit status.
func (s *smallService) compat(t *testing.T, args ...string) (string, int) {
	t.Helper()
	stdout, stderr, err := s.stego(t, append([]string{"compat"}, args...)...)
	return stdout + stderr, exitCode(t, err)
}

func (s *smallService) compatJSON(t *testing.T) (compatResult, int) {
	t.Helper()
	stdout, stderr, err := s.stego(t, "compat", "--base", "HEAD", "--json")
	var result compatResult
	if jsonErr := json.Unmarshal([]byte(stdout), &result); jsonErr != nil {
		t.Fatalf("stego compat --json printed no JSON object: %v\n%s%s", jsonErr, stdout, stderr)
	}
	return result, exitCode(t, err)
}

// noteFields gives the declaration of ST-GEN-TD03 with other fields of Note.
func noteFields(fields string) string {
	return strings.Replace(smallServiceDeclaration, "fields: [{name: title, type: string}]", "fields: "+fields, 1)
}

const (
	renamedTitle = "[{name: heading, type: string}]"
	titleRename  = "schema_changes:\n  - {id: note-title-rename, kind: rename, entity: Note, field: heading, from: title}\nschema_history:\n  - {change: note-title-rename, phase: expand}\n"
)

func TestCompat(t *testing.T) {
	t.Parallel()
	t.Run("ST-GEN-UC14-R01-T01 A base declaration that is not the one its record names is refused", func(t *testing.T) {
		t.Parallel()
		s := newSmallService(t)
		recorded := s.record(t)["last_applied"].(map[string]any)["service_hash"].(string)
		changed := strings.Replace(smallServiceDeclaration, "{name: title, type: string}", "{name: title, type: string, max_length: 80}", 1)
		s.write(t, "service.yaml", changed)
		git(t, s.service, "commit", "-q", "-am", "change max_length without apply")
		commit := git(t, s.service, "rev-parse", "HEAD")
		output, code := s.compat(t, "--base", "HEAD")
		if code != 1 {
			t.Fatalf("exit %d, want 1:\n%s", code, output)
		}
		sum := sha256.Sum256([]byte(changed))
		base := hex.EncodeToString(sum[:])
		for _, want := range []string{"HEAD", commit, recorded, base} {
			if !strings.Contains(output, want) {
				t.Errorf("the fault does not name %q:\n%s", want, output)
			}
		}
	})
	t.Run("ST-GEN-UC14-R02-T01 With no record at the base ref, any stored shape is accepted", func(t *testing.T) {
		t.Parallel()
		s := newSmallService(t)
		fresh := filepath.Join(t.TempDir(), "notes")
		if err := os.MkdirAll(fresh, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, fresh, "service.yaml", smallServiceDeclaration, 0o644)
		git(t, fresh, "init", "-q")
		git(t, fresh, "add", "-A")
		git(t, fresh, "commit", "-q", "-m", "first declaration, no record")
		writeFile(t, fresh, "service.yaml", noteFields("[{name: heading, type: string}, {name: owner, type: string}]"), 0o644)
		s.service = fresh
		output, code := s.compat(t, "--base", "HEAD")
		if code != 0 || !strings.Contains(output, "No fault: the base holds no record") {
			t.Fatalf("exit %d, want 0 and no fault:\n%s", code, output)
		}
	})
	t.Run("ST-GEN-UC14-R03-T01 Each fault names the entity, the field, the rule and the schema change and phase to declare", func(t *testing.T) {
		t.Parallel()
		s := newSmallService(t)
		heads := []struct {
			name, declaration, field string
			declare                  []string // kind, field, from, needs for each change to declare
		}{
			{"rename", noteFields(renamedTitle), "title", []string{"rename heading title "}},
			{"retype", noteFields("[{name: title, type: int32}]"), "title", []string{"retype title string a conversion and an inverse conversion"}},
			{"require", noteFields("[{name: title, type: string}, {name: owner, type: string}]"), "owner", []string{"require owner  a fill value"}},
			{"unique", smallServiceDeclaration + "invariants:\n  - {name: unique_title, kind: unique, on: Note, by: [title]}\n", "unique_title", []string{"unique unique_title  "}},
			{"drop and add", noteFields("[{name: body, type: string, optional: true}]"), "title", []string{"drop title  the Definition string", "add body  "}},
		}
		for _, head := range heads {
			s.write(t, "service.yaml", head.declaration)
			result, code := s.compatJSON(t)
			if code != 1 || len(result.Faults) != 1 {
				t.Errorf("%s: exit %d with %d fault(s), want 1 and one fault: %+v", head.name, code, len(result.Faults), result.Faults)
				continue
			}
			fault := result.Faults[0]
			if fault.Entity != "Note" || fault.Field != head.field || fault.Rule != "ST-GEN-INV33" {
				t.Errorf("%s: the fault names %s.%s and the rule %s, want Note.%s and ST-GEN-INV33", head.name, fault.Entity, fault.Field, fault.Rule, head.field)
			}
			var got []string
			for _, change := range fault.Declare {
				if change.Phase != "expand" {
					t.Errorf("%s: the change %s is at %q, want expand", head.name, change.ID, change.Phase)
				}
				got = append(got, change.Kind+" "+change.Field+" "+change.From+" "+change.Needs)
			}
			if strings.Join(got, "|") != strings.Join(head.declare, "|") {
				t.Errorf("%s: the fault declares %q, want %q", head.name, got, head.declare)
			}
		}
	})
	t.Run("ST-GEN-UC14-R04-T01 The check names the base ref and its commit with and without a fault", func(t *testing.T) {
		t.Parallel()
		s := newSmallService(t)
		commit := git(t, s.service, "rev-parse", "HEAD")
		output, code := s.compat(t, "--base", "HEAD")
		if code != 0 || !strings.Contains(output, "HEAD") || !strings.Contains(output, commit) {
			t.Errorf("unchanged: exit %d, want 0 and HEAD at %s:\n%s", code, commit, output)
		}
		s.write(t, "service.yaml", noteFields(renamedTitle))
		output, code = s.compat(t, "--base", "HEAD")
		if code != 1 || !strings.Contains(output, "HEAD") || !strings.Contains(output, commit) {
			t.Errorf("renamed: exit %d, want 1 and HEAD at %s:\n%s", code, commit, output)
		}
		result, code := s.compatJSON(t)
		if code != 1 || result.BaseRef != "HEAD" || result.BaseCommit != commit {
			t.Errorf("--json: exit %d, base ref %q and base commit %q, want 1, HEAD and %s", code, result.BaseRef, result.BaseCommit, commit)
		}
	})
	t.Run("ST-GEN-UC14-FL-T An undeclared rename is refused against the base, and the same rename as a schema change is accepted", func(t *testing.T) {
		t.Parallel()
		s := newSmallService(t)
		commit := git(t, s.service, "rev-parse", "HEAD")
		s.write(t, "service.yaml", noteFields(renamedTitle))
		output, code := s.compat(t, "--base", "HEAD")
		if code != 1 {
			t.Fatalf("undeclared rename: exit %d, want 1:\n%s", code, output)
		}
		for _, want := range []string{"Note.title", "ST-GEN-INV33", "rename note-title-rename (Note.heading from title) at expand", "HEAD", commit} {
			if !strings.Contains(output, want) {
				t.Errorf("undeclared rename: the output does not name %q:\n%s", want, output)
			}
		}
		s.write(t, "service.yaml", noteFields(renamedTitle)+titleRename)
		output, code = s.compat(t, "--base", "HEAD")
		if code != 0 || !strings.Contains(output, commit) {
			t.Errorf("declared rename: exit %d, want 0 and the commit %s:\n%s", code, commit, output)
		}
		fresh := filepath.Join(t.TempDir(), "notes")
		if err := os.MkdirAll(fresh, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, fresh, "service.yaml", smallServiceDeclaration, 0o644)
		git(t, fresh, "init", "-q")
		git(t, fresh, "add", "-A")
		git(t, fresh, "commit", "-q", "-m", "first release, no record")
		freshCommit := git(t, fresh, "rev-parse", "HEAD")
		writeFile(t, fresh, "service.yaml", noteFields(renamedTitle), 0o644)
		s.service = fresh
		output, code = s.compat(t, "--base", "HEAD")
		if code != 0 || !strings.Contains(output, freshCommit) {
			t.Errorf("first release: exit %d, want 0 and the commit %s:\n%s", code, freshCommit, output)
		}
	})
}
