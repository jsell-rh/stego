package compiler

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jsell-rh/stego/internal/registry"
)

const digestFixture = "testdata/component-digest"

// copyDigestFixture copies the fixture registry into a new folder. Reverse
// writes the files in reverse path order; mode and modified set each file's
// permission bits and modification time.
func copyDigestFixture(t *testing.T, reverse bool, mode fs.FileMode, modified time.Time) string {
	t.Helper()
	target := t.TempDir()
	var names []string
	if err := filepath.WalkDir(digestFixture, func(name string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			names = append(names, name)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	slices.Sort(names)
	if reverse {
		slices.Reverse(names)
	}
	for _, name := range names {
		relative, err := filepath.Rel(digestFixture, name)
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		writeDigestFile(t, target, relative, string(data))
		path := filepath.Join(target, relative)
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, modified, modified); err != nil {
			t.Fatal(err)
		}
	}
	return target
}

func writeDigestFile(t *testing.T, root, relative, data string) {
	t.Helper()
	path := filepath.Join(root, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func editDigestFile(t *testing.T, root, relative, old, replacement string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, relative))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), old) {
		t.Fatalf("%s does not hold %q", relative, old)
	}
	writeDigestFile(t, root, relative, strings.Replace(string(data), old, replacement, 1))
}

// digestOf captures the registry folder as Load does and gives the digest of
// one component.
func digestOf(t *testing.T, root, name string) string {
	t.Helper()
	files, err := registry.CaptureInputFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := ComponentDigest(files, name)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func TestComponentDigest(t *testing.T) {
	t.Run("ST-GEN-DS01-T01 The same component content gives the same digest in any folder and file order", func(t *testing.T) {
		first := copyDigestFixture(t, false, 0o644, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
		second := copyDigestFixture(t, true, 0o600, time.Date(2030, 6, 7, 8, 9, 10, 0, time.UTC))
		one, two := digestOf(t, first, "alpha"), digestOf(t, second, "alpha")
		if one != two {
			t.Fatalf("two copies give two digests: %s and %s", one, two)
		}
		// The fixed vector of the fixture component. A change to the
		// digest's codec changes it, and must come with a new codec name.
		const vector = "be2424ce89caff850036157bbcdb5dc04b0f04b4749ea6973927a098aaa2966d"
		if one != vector {
			t.Fatalf("the digest of the fixture component is %s, not the fixed vector %s", one, vector)
		}
	})

	t.Run("ST-GEN-DS01-T02 A change to another component never changes a component's digest", func(t *testing.T) {
		root := copyDigestFixture(t, false, 0o644, time.Now())
		before := digestOf(t, root, "alpha")
		editDigestFile(t, root, "components/beta/component.yaml", "version: 0.4.0", "version: 0.4.1")
		writeDigestFile(t, root, "archetypes/other/archetype.yaml", "kind: archetype\nname: other\nlanguage: go\nversion: 1.0.0\ncomponents: [beta]\n")
		editDigestFile(t, root, "registry.yaml", "component-digest-fixture", "component-digest-fixture-renamed")
		if after := digestOf(t, root, "alpha"); after != before {
			t.Fatalf("a change outside component alpha changed its digest from %s to %s", before, after)
		}
		editDigestFile(t, root, "components/alpha/slots/check.proto", "string name = 1;", "string name = 2;")
		if after := digestOf(t, root, "alpha"); after == before {
			t.Fatal("a change to a file of component alpha left its digest as it was")
		}
	})

	t.Run("ST-GEN-DS01-T03 A change of the stage or the sunset alone leaves a component's digest as it was, and a change of any other field of its file changes it", func(t *testing.T) {
		root := copyDigestFixture(t, false, 0o644, time.Now())
		const file = "components/alpha/component.yaml"
		first := digestOf(t, root, "alpha")

		editDigestFile(t, root, file, "stage: beta", "stage: alpha")
		second := digestOf(t, root, "alpha")

		editDigestFile(t, root, file, "stage: alpha\nsunset: 2027-06-30\n", "stage: ga\n")
		third := digestOf(t, root, "alpha")

		editDigestFile(t, root, file, "stage: ga\n", "stage: beta\nsunset: 2027-06-30\n")
		if restored := digestOf(t, root, "alpha"); restored != first {
			t.Fatalf("the restored stage and sunset give %s, not the first digest %s", restored, first)
		}
		editDigestFile(t, root, file, "Serves the sample requests", "Serves the sample calls")
		fourth := digestOf(t, root, "alpha")

		editDigestFile(t, root, file, "Serves the sample calls", "Serves the sample requests")
		editDigestFile(t, root, file, "version: 1.2.0", "version: 1.2.1")
		fifth := digestOf(t, root, "alpha")

		if second != first || third != first {
			t.Fatalf("a change of the stage or the sunset alone changed the digest: %s, %s, %s", first, second, third)
		}
		if fourth == first || fifth == first || fourth == fifth {
			t.Fatalf("a change of the description or the version did not give its own digest: %s, %s, %s", first, fourth, fifth)
		}
	})
}
