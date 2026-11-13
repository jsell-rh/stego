package main

import (
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// noteBody gives Note of ST-GEN-TD03 an optional field body.
func noteBody(declaration string) string {
	return strings.Replace(declaration, "fields: [{name: title, type: string}]", "fields: [{name: title, type: string}, {name: body, type: string, optional: true}]", 1)
}

// copyService copies the service folder, with its Git repository, beside it and gives the fixture of the copy.
func (s *smallService) copyService(t *testing.T, name string) *smallService {
	t.Helper()
	copied := *s
	copied.service = filepath.Join(s.parent, name)
	if output, err := exec.Command("cp", "-a", s.service, copied.service).CombinedOutput(); err != nil {
		t.Fatalf("copy the service folder: %v\n%s", err, output)
	}
	return &copied
}

func TestSchemaStepPlan(t *testing.T) {
	t.Parallel()
	t.Run("ST-GEN-UC12-R05-T01 Plan and apply refuse an undeclared change and write nothing", func(t *testing.T) {
		t.Parallel()
		s := newSmallService(t)
		s.must(t, "plan")
		s.write(t, "service.yaml", noteFields(renamedTitle))
		before := tree(t, s.service)
		for _, args := range [][]string{{"plan"}, {"apply", "--base", "HEAD"}} {
			stdout, stderr, err := s.stego(t, args...)
			output := stdout + stderr
			if err == nil {
				t.Errorf("stego %s exits 0 on an undeclared rename:\n%s", args[0], output)
			}
			for _, want := range []string{"Note.title", "rename note-title-rename (Note.heading from title) at expand"} {
				if !strings.Contains(output, want) {
					t.Errorf("stego %s does not name %q:\n%s", args[0], want, output)
				}
			}
			if after := tree(t, s.service); !maps.Equal(before, after) {
				t.Errorf("stego %s changed a file of the service folder", args[0])
			}
		}
	})
	t.Run("ST-GEN-UC12-R05-T01 With no --base, plan takes the default branch, refuses a crafted origin/HEAD and says when there is no base", func(t *testing.T) {
		t.Parallel()
		s := newSmallService(t)
		commit := git(t, s.service, "rev-parse", "HEAD")
		git(t, s.service, "tag", "v1")
		for _, c := range []struct{ target, want string }{
			{"refs/tags/v1", "which is no branch of origin"},
			{"refs/heads/main", "which is no branch of origin"},
			{"refs/remotes/origin/gone", "which names no commit"},
		} {
			git(t, s.service, "symbolic-ref", "refs/remotes/origin/HEAD", c.target)
			stdout, stderr, err := s.stego(t, "plan")
			if err == nil || !strings.Contains(stdout+stderr, c.want) {
				t.Errorf("origin/HEAD at %s: %v, want a refusal that says %q:\n%s%s", c.target, err, c.want, stdout, stderr)
			}
		}
		git(t, s.service, "update-ref", "refs/remotes/origin/main", commit)
		git(t, s.service, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
		if planned := s.must(t, "plan"); !strings.Contains(planned, "Base: refs/remotes/origin/main at commit "+commit) {
			t.Errorf("plan does not take the branch that origin/HEAD names:\n%s", planned)
		}
		git(t, s.service, "symbolic-ref", "--delete", "refs/remotes/origin/HEAD")
		if planned := s.must(t, "plan"); !strings.Contains(planned, "Base: refs/heads/main at commit "+commit) {
			t.Errorf("with no origin, plan does not take the local main:\n%s", planned)
		}
		git(t, s.service, "branch", "-m", "trunk")
		if stdout, stderr, err := s.stego(t, "plan"); err == nil || !strings.Contains(stdout+stderr, "has no default branch") {
			t.Errorf("a repository with no default branch: %v, want a refusal that asks for --base:\n%s%s", err, stdout, stderr)
		}
		git(t, s.service, "branch", "-m", "main")
		noGit := *s
		noGit.env = append(slices.Clone(s.env[1:]), "PATH="+filepath.Dir(stegoBinary(t)))
		if stdout, stderr, err := noGit.stego(t, "plan"); err == nil || !strings.Contains(stdout+stderr, "git cannot read the repository") {
			t.Errorf("with no git to run: %v, want a refusal, not a plan with no check:\n%s%s", err, stdout, stderr)
		}
		s.unreleased(t)
		if planned := s.must(t, "plan"); !strings.Contains(planned, "Base: refs/heads/main at commit "+git(t, s.service, "rev-parse", "main")+" holds no record") {
			t.Errorf("plan on a base with no record does not name the base and its commit:\n%s", planned)
		}
		other := s.copyService(t, "notes-no-git")
		if output, err := exec.Command("rm", "-rf", filepath.Join(other.service, ".git")).CombinedOutput(); err != nil {
			t.Fatalf("remove the repository: %v\n%s", err, output)
		}
		other.write(t, "service.yaml", noteFields(renamedTitle))
		if planned := other.must(t, "plan"); !strings.Contains(planned, "No base check") {
			t.Errorf("plan in a folder with no repository does not say that it has no base:\n%s", planned)
		}
		// The service folder seen through a symbolic link from a folder with no .git, while its real parent holds a
		// .git that git cannot read: git walks the real path, so the check must stop too.
		linked := filepath.Join(t.TempDir(), "linked")
		holder := filepath.Join(s.parent, "holder")
		if err := os.MkdirAll(holder, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, holder, ".git", "gitdir: "+filepath.Join(s.parent, "removed-worktree")+"\n", 0o644)
		if output, err := exec.Command("cp", "-a", other.service, filepath.Join(holder, "notes")).CombinedOutput(); err != nil {
			t.Fatalf("copy the service folder: %v\n%s", err, output)
		}
		if err := os.Symlink(filepath.Join(holder, "notes"), linked); err != nil {
			t.Fatal(err)
		}
		through := *other
		through.service = linked
		through.env = append(slices.Clone(other.env), "PWD="+linked)
		if stdout, stderr, err := through.stego(t, "plan"); err == nil || !strings.Contains(stdout+stderr, "git cannot read the repository") {
			t.Errorf("a linked folder whose real parent holds a .git that git cannot read: %v, want a refusal:\n%s%s", err, stdout, stderr)
		}
		writeFile(t, other.service, ".git", "gitdir: "+filepath.Join(s.parent, "removed-worktree")+"\n", 0o644)
		if stdout, stderr, err := other.stego(t, "plan"); err == nil || !strings.Contains(stdout+stderr, "git cannot read the repository") {
			t.Errorf("a .git link to a removed worktree: %v, want a refusal, not a plan with no check:\n%s%s", err, stdout, stderr)
		}
	})
	t.Run("ST-GEN-UC12-R06-T01 Apply writes the same add steps for a new entity and a new optional field each time", func(t *testing.T) {
		t.Parallel()
		s := newSmallService(t)
		changed := noteBody(secondEntityDeclaration)
		s.write(t, "service.yaml", changed)
		other := s.copyService(t, "notes-copy")
		before := tree(t, s.service)
		planned := s.must(t, "plan", "--base", "HEAD")
		for _, want := range []string{"add-note-body", "add-tag"} {
			if !strings.Contains(planned, want) {
				t.Errorf("plan does not list %s:\n%s", want, planned)
			}
		}
		if !maps.Equal(before, tree(t, s.service)) {
			t.Error("plan changed a file")
		}
		applied := s.must(t, "apply", "--base", "HEAD")
		for _, want := range []string{"add-note-body", "add-tag"} {
			if !strings.Contains(applied, want) {
				t.Errorf("apply does not name %s:\n%s", want, applied)
			}
		}
		declaration := s.read(t, "service.yaml")
		wantTail := "schema_changes:\n  - {id: add-note-body, kind: add, entity: Note, field: body}\n  - {id: add-tag, kind: add, entity: Tag}\n" +
			"schema_history:\n  - {change: add-note-body, phase: expand}\n  - {change: add-tag, phase: expand}\n"
		if !strings.HasPrefix(declaration, changed) || !strings.HasSuffix(declaration, wantTail) {
			t.Errorf("apply wrote service.yaml as:\n%s\nwant the declaration with this end:\n%s", declaration, wantTail)
		}
		recorded := s.record(t)["last_applied"].(map[string]any)["service_hash"]
		if want := sha256Text(declaration); recorded != want {
			t.Errorf("the record names the declaration digest %v, want %s, the digest of the new service.yaml", recorded, want)
		}
		other.must(t, "apply", "--base", "HEAD")
		if again := other.read(t, "service.yaml"); again != declaration {
			t.Errorf("a second apply of the same declaration wrote other steps:\n%s\nwant:\n%s", again, declaration)
		}
		required := strings.Replace(declaration, "{name: body, type: string, optional: true}", "{name: body, type: string, optional: true}, {name: owner, type: string}", 1)
		s.write(t, "service.yaml", required)
		stdout, stderr, err := s.stego(t, "apply", "--base", "HEAD")
		if err == nil || !strings.Contains(stdout+stderr, "require note-owner-require (Note.owner) with a fill value at expand") {
			t.Errorf("apply of a new required field with no default: %v, want a refusal that names the require to declare:\n%s%s", err, stdout, stderr)
		}
		if s.read(t, "service.yaml") != required {
			t.Error("the refused apply wrote a step in service.yaml")
		}
	})
	t.Run("ST-GEN-UC12-R02-T01 Plan and apply refuse a declared schema change whose kind generation does not carry out yet", func(t *testing.T) {
		t.Parallel()
		s := newSmallService(t)
		s.write(t, "service.yaml", noteFields(renamedTitle)+titleRename)
		before := tree(t, s.service)
		for _, command := range []string{"plan", "apply"} {
			stdout, stderr, err := s.stego(t, command, "--base", "HEAD")
			if err == nil || !strings.Contains(stdout+stderr, "does not carry out the schema change note-title-rename (rename) yet") {
				t.Errorf("stego %s of a declared rename: %v, want a refusal that names the change and its kind:\n%s%s", command, err, stdout, stderr)
			}
		}
		if !maps.Equal(before, tree(t, s.service)) {
			t.Error("a refused plan or apply changed a file")
		}
		if stdout, stderr, err := s.stego(t, "validate"); err != nil {
			t.Errorf("stego validate refuses the declared rename, which checking accepts: %v\n%s%s", err, stdout, stderr)
		}
	})
	t.Run("ST-GEN-UC12-R06-T02 Without apply, the check refuses a new field that has no add step", func(t *testing.T) {
		t.Parallel()
		s := newSmallService(t)
		changed := noteBody(smallServiceDeclaration)
		s.write(t, "service.yaml", changed)
		result, code := s.compatJSON(t)
		if code != 1 || len(result.Faults) != 1 {
			t.Fatalf("exit %d with %+v, want 1 and one fault", code, result.Faults)
		}
		fault := result.Faults[0]
		if fault.Entity != "Note" || fault.Field != "body" || fault.Rule != "ST-GEN-INV33" || len(fault.Declare) != 1 ||
			fault.Declare[0].ID != "add-note-body" || fault.Declare[0].Kind != "add" || fault.Declare[0].Phase != "expand" {
			t.Errorf("the fault is %+v, want Note, body, ST-GEN-INV33 and add-note-body of kind add at expand", fault)
		}
		if s.read(t, "service.yaml") != changed {
			t.Error("stego compat changed service.yaml")
		}
	})
}

// sha256Text gives the SHA-256 of a text in hexadecimal.
func sha256Text(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
