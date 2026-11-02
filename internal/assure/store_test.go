package assure

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitRepository makes a git repository with a committed spec: lathe.config.json, a table with no tests, and a
// table with one automated test record. It returns the repository's folder.
func gitRepository(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "repository")
	for name, content := range map[string]string{
		"lathe.config.json":         `{"schema": 5, "data": "data"}` + "\n",
		"data/domain.jsonl":         `{"id":"ST-FIX"}` + "\n",
		"data/invariant_test.jsonl": `{"id":"ST-FIX-INV01-T","state":"automated","file":"internal/fix/fix_test.go"}` + "\n",
		"internal/fix/fix.go":       "package fix\n",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"add", "-A"},
		{"-c", "user.name=fixture", "-c", "user.email=fixture@example.com", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "fixture"},
	} {
		gitIn(t, root, args...)
	}
	return root
}

func gitIn(t *testing.T, root string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", root}, args...)...)
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}

func fixtureRun() ProofRun {
	return ProofRun{
		Commit:       fixtureCommit,
		Provider:     "github-actions",
		RunReference: "github:35950507614/1",
		SpecDigest:   "sha256:" + strings.Repeat("a", 64),
		Outcomes:     []Outcome{{TestRecord: "ST-FIX-INV01-T", Result: Passed, File: "internal/fix/fix_test.go"}},
	}
}

func TestOutsideTheRepository(t *testing.T) {
	t.Run("ST-ASR-INV05-T A proof run is never written as a file in the product's repository", func(t *testing.T) {
		base := t.TempDir()
		repository := filepath.Join(base, "repository")
		outside := filepath.Join(base, "runner-temp")
		for _, folder := range []string{filepath.Join(repository, "data"), outside} {
			if err := os.MkdirAll(folder, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Symlink(filepath.Join(repository, "data"), filepath.Join(outside, "link-to-data")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(repository, "data", "kept.json"), filepath.Join(outside, "link-to-file.json")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repository, "data", "kept.json"), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		for name, file := range map[string]string{
			"the repository's root":                      filepath.Join(repository, ProofRunFile),
			"a folder in the repository":                 filepath.Join(repository, "data", ProofRunFile),
			"a path that climbs back in":                 filepath.Join(outside, "..", "repository", ProofRunFile),
			"a link to a folder in the repository":       filepath.Join(outside, "link-to-data", ProofRunFile),
			"a link to a file in the repository":         filepath.Join(outside, "link-to-file.json"),
			"a relative path from inside the repository": relativeFrom(t, repository, filepath.Join(repository, ProofRunFile)),
		} {
			store := GitHubAttestations{Repository: repository, Predicate: file}
			_, err := store.HandOver(fixtureRun())
			var refusal *Refusal
			if !errors.As(err, &refusal) || refusal.Rule != "outsideTheRepository" {
				t.Fatalf("%s (%s): got %v, want a refusal by outsideTheRepository", name, file, err)
			}
		}
		if data, err := os.ReadFile(filepath.Join(repository, "data", "kept.json")); err != nil || string(data) != "{}" {
			t.Fatalf("a refused write changed a file in the repository: %q, %v", data, err)
		}
		entries, err := filepath.Glob(filepath.Join(repository, "*", ProofRunFile))
		if err != nil || len(entries) != 0 {
			t.Fatalf("a refused write left %v in the repository", entries)
		}
		if err := outsideTheRepository(repository, filepath.Join(outside, ProofRunFile)); err != nil {
			t.Fatalf("a file outside the repository is refused: %v", err)
		}
		if err := outsideTheRepository(repository, filepath.Join(base, "repository-sibling", ProofRunFile)); err == nil {
			t.Fatal("a file in a folder that does not exist is accepted")
		}
	})
}

// relativeFrom is the file's path relative to the working folder, which the test moves to folder.
func relativeFrom(t *testing.T, folder, file string) string {
	t.Helper()
	t.Chdir(folder)
	relative, err := filepath.Rel(folder, file)
	if err != nil {
		t.Fatal(err)
	}
	return relative
}

func TestAttestationStore(t *testing.T) {
	t.Run("ST-ASR-A1-SR01-T01 A proof run is kept only outside the repository for the provider's attestation, and never committed", func(t *testing.T) {
		repository := gitRepository(t)
		before := gitIn(t, repository, "rev-parse", "HEAD")
		predicate := filepath.Join(t.TempDir(), ProofRunFile)
		store := GitHubAttestations{Repository: repository, Predicate: predicate}
		run := fixtureRun()
		digest, err := store.HandOver(run)
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(predicate)
		if err != nil {
			t.Fatalf("the predicate is not where the attestation job reads it: %v", err)
		}
		sum := sha256.Sum256(data)
		if digest != "sha256:"+hex.EncodeToString(sum[:]) {
			t.Fatalf("digest %s is not the predicate's", digest)
		}
		if !bytes.Contains(data, []byte(`"commit": "`+fixtureCommit+`"`)) {
			t.Fatalf("the predicate does not name the commit:\n%s", data)
		}
		if status := gitIn(t, repository, "status", "--porcelain", "--untracked-files=all", "--ignored"); status != "" {
			t.Fatalf("the repository changed: %s", status)
		}
		if after := gitIn(t, repository, "rev-parse", "HEAD"); after != before {
			t.Fatalf("a commit was made: %s, then %s", before, after)
		}

		inside := GitHubAttestations{Repository: repository, Predicate: filepath.Join(repository, "data", ProofRunFile)}
		if _, err := inside.HandOver(run); err == nil {
			t.Fatal("the store keeps a proof run in the repository")
		}
		if status := gitIn(t, repository, "status", "--porcelain", "--untracked-files=all", "--ignored"); status != "" {
			t.Fatalf("a refused proof run changed the repository: %s", status)
		}
	})
}
