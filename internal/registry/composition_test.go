package registry

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRegistryCompositionKeepsDistinctSourcesAndSnapshots(t *testing.T) {
	common := snapshotFixture(t)
	local := t.TempDir()
	name := filepath.Join(local, "archetypes", "application", "archetype.yaml")
	if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte("kind: archetype\nname: application\nlanguage: go\nversion: 1.0.0\ncomponents: [sample]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	combined, err := LoadDirectories(common, local)
	if err != nil {
		t.Fatal(err)
	}
	if combined.Component("sample") == nil || combined.Archetype("application") == nil {
		t.Fatal("composed artifact is missing")
	}
	reversed, err := LoadDirectories(local, common)
	if err != nil || reversed.ContentHash() != combined.ContentHash() {
		t.Fatal("source order changed the content hash", err)
	}
	if err := combined.Verify(); err != nil {
		t.Fatal(err)
	}
	original, err := combined.ReadProtoImport("stego/common/types.proto")
	if err != nil || len(original) == 0 {
		t.Fatal("common import missing", err)
	}
	if err := os.WriteFile(name, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := combined.Verify(); err == nil {
		t.Fatal("local registry change was not detected")
	}
	if combined.Archetype("application").Name != "application" {
		t.Fatal("captured local declaration changed")
	}
}

func TestRegistryCompositionRejectsShadowingAndSourceCount(t *testing.T) {
	for _, scenario := range []string{"same root", "duplicate artifact", "duplicate input", "empty artifact"} {
		t.Run(scenario, func(t *testing.T) {
			common := snapshotFixture(t)
			local := t.TempDir()
			switch scenario {
			case "same root":
				local = common
			case "duplicate artifact":
				local = snapshotFixture(t)
			case "duplicate input":
				if err := os.MkdirAll(filepath.Join(local, "common"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(local, "common/types.proto"), []byte("syntax = \"proto3\";\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "empty artifact":
				if err := os.MkdirAll(filepath.Join(local, "components/sample"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := LoadDirectories(common, local); err == nil {
				t.Fatal("registry shadow accepted")
			}
		})
	}
	if _, err := LoadDirectories(); err == nil {
		t.Fatal("empty source list accepted")
	}
	dirs := make([]string, 9)
	for i := range dirs {
		dirs[i] = t.TempDir()
	}
	if _, err := LoadDirectories(dirs...); err == nil {
		t.Fatal("excess source list accepted")
	}
}
