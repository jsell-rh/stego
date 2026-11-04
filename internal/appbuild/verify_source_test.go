package appbuild

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sourceFixture(t *testing.T) (string, string, string) {
	t.Helper()
	source := t.TempDir()
	put(t, source, ".stego/state.yaml", "recorded state")
	put(t, source, "out/main.go", "package main\nfunc main() {}\n")
	record := fixtureRecord()
	var err error
	record.Source, record.Inputs, err = inventory(source, maxFiles, maxBytes)
	if err != nil {
		t.Fatal(err)
	}
	record.GenerationStateSHA256 = digest([]byte("recorded state"))
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	name := filepath.Join(t.TempDir(), "build.json")
	if err := os.WriteFile(name, data, 0600); err != nil {
		t.Fatal(err)
	}
	return source, name, digest(data)
}

func TestVerifySourceRejectsFileSetContentAndModeChanges(t *testing.T) {
	t.Run("A source snapshot is refused on any changed, missing or added file", func(t *testing.T) {
		for _, change := range []string{"content", "missing", "added", "mode", "link", "git metadata"} {
			t.Run(change, func(t *testing.T) {
				source, record, hash := sourceFixture(t)
				if _, err := VerifySource(record, hash, source); err != nil {
					t.Fatal("original source failed", err)
				}
				name := filepath.Join(source, "out/main.go")
				var err error
				switch change {
				case "content":
					err = os.WriteFile(name, []byte("package main\nfunc main() { panic(1) }\n"), 0600)
				case "missing":
					err = os.Remove(name)
				case "added":
					put(t, source, "out/extra.go", "package main\nfunc init() { panic(1) }\n")
				case "mode":
					err = os.Chmod(name, 0700)
				case "link":
					outside := t.TempDir()
					put(t, outside, "same.go", "package main\nfunc main() {}\n")
					if err = os.Remove(name); err == nil {
						err = os.Symlink(filepath.Join(outside, "same.go"), name)
					}
				case "git metadata":
					put(t, source, ".git/config", "extra")
				}
				if err != nil {
					t.Fatal(err)
				}
				if _, err := VerifySource(record, hash, source); err == nil {
					t.Fatal("changed source accepted")
				}
			})
		}
	})
}

func TestVerifySourceRequiresTrustedRecordAndRealRoot(t *testing.T) {
	t.Run("A source check refuses an untrusted record digest", func(t *testing.T) {
		source, record, hash := sourceFixture(t)
		if _, err := VerifySource(record, strings.Repeat("0", 64), source); err == nil {
			t.Fatal("wrong trusted digest accepted")
		}
		link := filepath.Join(t.TempDir(), "linked-source")
		if err := os.Symlink(source, link); err != nil {
			t.Fatal(err)
		}
		for _, root := range []string{"", record, link, link + string(os.PathSeparator), filepath.Join(source, "missing")} {
			if _, err := VerifySource(record, hash, root); err == nil {
				t.Fatal("invalid root accepted", root)
			}
		}
		if _, err := VerifySource(record, hash, source); err != nil {
			t.Fatal("valid source failed after rejections", err)
		}
	})
}

func TestSourceMismatchesNameEachPath(t *testing.T) {
	t.Run("ST-BLD-DS02-T02 A source snapshot is refused on any changed, missing or added file, and each is named", func(t *testing.T) {
		hash := func(c string) string { return strings.Repeat(c, 64) }
		recorded := []File{{"a.go", hash("a"), false}, {"b.go", hash("b"), false}, {"c.go", hash("c"), true}, {"d.go", hash("d"), false}}
		if mismatches := sourceMismatches(recorded, recorded); len(mismatches) != 0 {
			t.Fatalf("the recorded files were refused: %v", mismatches)
		}
		found := []File{{"a.go", hash("a"), false}, {"b.go", hash("0"), false}, {"c.go", hash("c"), false}, {"e.go", hash("e"), false}}
		mismatches := strings.Join(sourceMismatches(recorded, found), "\n")
		for _, want := range []string{"b.go: changed, recorded " + hash("b") + ", found " + hash("0"), "c.go: executable bit recorded true, found false", "d.go: missing", "e.go: added"} {
			if !strings.Contains(mismatches, want) {
				t.Errorf("the check does not name %q:\n%s", want, mismatches)
			}
		}
		if strings.Contains(mismatches, "a.go") {
			t.Error("the check names a file that matches")
		}
	})
}
