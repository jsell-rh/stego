package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRegistryCompositionKeepsDistinctSourcesAndSnapshots(t *testing.T) {
	common := snapshotFixture(t)
	local := t.TempDir()
	name := filepath.Join(local, "archetypes", "application", "archetype.yaml")
	if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte("kind: archetype\nruntime: {module: github.com/jsell-rh/stego/registry/runtime, version: v0.1.0, sum: 'h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA='}\nname: application\nlanguage: go\nversion: 1.0.0\ncomponents: [sample]\n"), 0600); err != nil {
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

// inv01CommonFiles are the parts of a small common registry: a component, an
// archetype, a mixin with its slot contract, and a port.
func inv01CommonFiles() map[string]string {
	return map[string]string{
		"components/store-a/component.yaml":          "kind: component\nname: store-a\nversion: 1.0.0\nstage: ga\noutput_namespace: internal/store\nrequires: []\nprovides: [store-port]\nslots: []\n",
		"archetypes/svc-arch/archetype.yaml":         "kind: archetype\nruntime: {module: github.com/jsell-rh/stego/registry/runtime, version: v0.1.0, sum: 'h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA='}\nname: svc-arch\nlanguage: go\nversion: 1.0.0\ncomponents: [store-a]\n",
		"mixins/extra-mixin/mixin.yaml":              "kind: mixin\nname: extra-mixin\nversion: 1.0.0\nadds_components: [store-a]\nadds_slots:\n  - name: after_extra\n    proto: stego.mixins.extra_mixin.slots.AfterExtra\noverrides: none\n",
		"mixins/extra-mixin/slots/after_extra.proto": "syntax = \"proto3\";\n\nservice AfterExtra {}\n",
		"ports/store-port/port.yaml":                 "kind: port\nname: store-port\n",
	}
}

func writeRegistryFolder(t *testing.T, files map[string]string) string {
	t.Helper()
	folder := t.TempDir()
	for name, data := range files {
		target := filepath.Join(folder, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return folder
}

func TestLocalRegistryAddsAndNeverReplaces(t *testing.T) {
	t.Run("ST-SUP-INV01-T A local part with the name of a common part is refused, and a new local part is added", func(t *testing.T) {
		commonFiles := inv01CommonFiles()
		common := Source{Name: "common-registry at c0ffee", Dir: writeRegistryFolder(t, commonFiles)}
		for _, part := range []struct{ kind, name string }{{"component", "store-a"}, {"archetype", "svc-arch"}, {"mixin", "extra-mixin"}, {"port", "store-port"}} {
			for _, changed := range []bool{false, true} {
				files := map[string]string{}
				for name, data := range commonFiles {
					if strings.HasPrefix(name, part.kind+"s/"+part.name+"/") {
						if changed && strings.HasSuffix(name, ".yaml") {
							data += "# changed\n"
						}
						files[name] = data
					}
				}
				local := Source{Name: "local-registry at beef01", Dir: writeRegistryFolder(t, files)}
				active, err := LoadSources(common, local)
				if err == nil || active != nil {
					t.Fatalf("%s %s (changed %v) replaced a common part", part.kind, part.name, changed)
				}
				for _, word := range []string{part.kind + ` "` + part.name + `"`, common.Name, local.Name} {
					if !strings.Contains(err.Error(), word) {
						t.Fatalf("the refusal does not name %q: %v", word, err)
					}
				}
			}
		}
		local := Source{Name: "local-registry at beef01", Dir: writeRegistryFolder(t, map[string]string{
			"components/local-a/component.yaml": "kind: component\nname: local-a\nversion: 0.1.0\nstage: ga\noutput_namespace: internal/local\nrequires: []\nprovides: [local-port]\nslots: []\n",
		})}
		active, err := LoadSources(common, local)
		if err != nil {
			t.Fatal(err)
		}
		for name, data := range commonFiles {
			held, err := active.ReadFile(name)
			if err != nil || string(held) != data {
				t.Fatalf("the active registry does not hold %s as the common registry gives it: %v", name, err)
			}
		}
		if active.Component("store-a") == nil || active.Archetype("svc-arch") == nil || active.Mixin("extra-mixin") == nil || active.Component("local-a") == nil {
			t.Fatal("the active registry lacks a common part or local-a")
		}
	})
}
