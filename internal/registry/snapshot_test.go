package registry

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func snapshotFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, data := range map[string]string{
		"components/sample/component.yaml":    "kind: component\nname: sample\nversion: 1.0.0\nslots:\n  - name: check\n    proto: sample.Check\n",
		"components/sample/slots/check.proto": "syntax = \"proto3\";\n",
		"common/types.proto":                  "syntax = \"proto3\";\n// Common input.\n",
	} {
		file := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestRegistrySnapshotIsImmutableAndPortable(t *testing.T) {
	first, second := snapshotFixture(t), snapshotFixture(t)
	a, err := Load(first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Load(second)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.ContentHash()) != 64 || a.ContentHash() != b.ContentHash() {
		t.Fatal("registry location changed content identity")
	}
	original, err := a.ReadProtoImport("stego/common/types.proto")
	if err != nil {
		t.Fatal(err)
	}
	altered, err := a.ReadFile("common/types.proto")
	if err != nil {
		t.Fatal(err)
	}
	altered[0] = 'x'
	again, err := a.ReadFile("common/types.proto")
	if err != nil || !bytes.Equal(original, again) {
		t.Fatal("caller changed snapshot", err)
	}
	if err := os.WriteFile(filepath.Join(first, "common/types.proto"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	captured, err := a.ReadFile("common/types.proto")
	if err != nil || !bytes.Equal(captured, original) {
		t.Fatal("read changed after capture", err)
	}
	if err := a.Verify(); err == nil {
		t.Fatal("changed protobuf was not detected")
	}
	c, err := Load(first)
	if err != nil {
		t.Fatal(err)
	}
	if c.ContentHash() == a.ContentHash() {
		t.Fatal("changed content kept the old identity")
	}
	if err := b.Verify(); err != nil {
		t.Fatal(err)
	}
}

func TestRegistrySnapshotDetectsInputSetChanges(t *testing.T) {
	for _, change := range []string{"metadata", "add proto", "remove proto", "add directory", "replace with link", "category file"} {
		t.Run(change, func(t *testing.T) {
			dir := snapshotFixture(t)
			registry, err := Load(dir)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "metadata":
				err = os.WriteFile(filepath.Join(dir, "components/sample/component.yaml"), []byte("# changed\n"), 0600)
			case "add proto":
				err = os.WriteFile(filepath.Join(dir, "common/new.proto"), nil, 0600)
			case "remove proto":
				err = os.Remove(filepath.Join(dir, "common/types.proto"))
			case "add directory":
				err = os.Mkdir(filepath.Join(dir, "components/new"), 0700)
			case "replace with link":
				file := filepath.Join(dir, "common/types.proto")
				if err = os.Remove(file); err == nil {
					err = os.Symlink("../components/sample/slots/check.proto", file)
				}
			case "category file":
				err = os.WriteFile(filepath.Join(dir, "mixins"), nil, 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := registry.Verify(); err == nil {
				t.Fatal("registry change was accepted")
			}
		})
	}
}

func TestRegistrySnapshotExcludesNonInputs(t *testing.T) {
	dir := snapshotFixture(t)
	a, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("documentation"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git/ignored.yaml"), []byte("not registry input"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.Verify(); err != nil {
		t.Fatal(err)
	}
	b, err := Load(dir)
	if err != nil || a.ContentHash() != b.ContentHash() {
		t.Fatal("non-input changed the content identity", err)
	}
	for _, name := range []string{"../outside.proto", "/outside.proto", "common/../types.proto", "common\\types.proto", "README.md", ".git/ignored.yaml"} {
		if _, err := a.ReadProtoImport(name); err == nil {
			t.Fatal("invalid import was accepted", name)
		}
	}
}

func TestRegistrySnapshotBounds(t *testing.T) {
	t.Run("file bytes", func(t *testing.T) {
		dir := t.TempDir()
		file, err := os.Create(filepath.Join(dir, "large.proto"))
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Truncate(4<<20 + 1); err != nil {
			t.Fatal(err)
		}
		file.Close()
		if _, err := captureRegistry(dir, true); err == nil {
			t.Fatal("oversized input was accepted")
		}
	})
	t.Run("total bytes", func(t *testing.T) {
		dir := t.TempDir()
		for i := 0; i < 17; i++ {
			file, err := os.Create(filepath.Join(dir, fmt.Sprintf("%02d.proto", i)))
			if err != nil {
				t.Fatal(err)
			}
			if err := file.Truncate(4 << 20); err != nil {
				t.Fatal(err)
			}
			file.Close()
		}
		if _, err := captureRegistry(dir, false); err == nil {
			t.Fatal("oversized registry was accepted")
		}
	})
	t.Run("file count", func(t *testing.T) {
		dir := t.TempDir()
		for i := 0; i < maxRegistryFiles+1; i++ {
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%05d.proto", i)), nil, 0600); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := captureRegistry(dir, false); err == nil {
			t.Fatal("excess input files were accepted")
		}
	})
	t.Run("directory entries", func(t *testing.T) {
		dir := t.TempDir()
		for i := 0; i < maxRegistryEntries+1; i++ {
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%05d.md", i)), nil, 0600); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := captureRegistry(dir, false); err == nil || !strings.Contains(err.Error(), "directory entries") {
			t.Fatal("excess entries were accepted", err)
		}
	})
	t.Run("directory depth", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, strings.Repeat("level/", maxRegistryDepth+1)), 0700); err != nil {
			t.Fatal(err)
		}
		if _, err := captureRegistry(dir, false); err == nil {
			t.Fatal("excess directory depth was accepted")
		}
	})
	t.Run("symbolic root", func(t *testing.T) {
		dir := snapshotFixture(t)
		link := filepath.Join(t.TempDir(), "linked")
		if err := os.Symlink(dir, link); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(link); err == nil {
			t.Fatal("symbolic registry root was accepted")
		}
	})
}

func TestRegistryHashIncludesNamesAndEqualLengthContent(t *testing.T) {
	dir := snapshotFixture(t)
	original, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "common/types.proto")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-2] = 'X'
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	changed, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if changed.ContentHash() == original.ContentHash() {
		t.Fatal("equal-length content change kept the old digest")
	}
	if err := os.Rename(file, filepath.Join(dir, "common/renamed.proto")); err != nil {
		t.Fatal(err)
	}
	renamed, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if renamed.ContentHash() == changed.ContentHash() {
		t.Fatal("renamed input kept the old digest")
	}
}
