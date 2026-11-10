package registry_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/mod/modfile"

	"github.com/jsell-rh/stego/internal/registry"
)

// pinArchetype is an archetype document whose runtime block is the text given.
func pinArchetype(runtime string) string {
	return "kind: archetype\nname: svc-arch\nlanguage: go\nversion: 1.0.0\ncomponents: [store-a]\n" + runtime
}

func TestRuntimePin(t *testing.T) {
	t.Run("ST-VO58-C01-T A runtime pin names a module path, a tagged version and its checksum, never a branch, a pseudo-version or a replace", func(t *testing.T) {
		sum := "h1:2jc7mdW5JmAEJw0KYdJKSGW/vx5dqWB1LfkmGdo9GNk="
		accepted, err := registry.ParseArchetypeFromBytes([]byte(pinArchetype("runtime:\n  module: stego.dev/runtime\n  version: v0.4.1\n  sum: "+sum+"\n")), "archetype.yaml")
		if err != nil {
			t.Fatal(err)
		}
		if accepted.Runtime == nil || *accepted.Runtime != (registry.RuntimePin{Module: "stego.dev/runtime", Version: "v0.4.1", Sum: sum}) {
			t.Fatalf("pin = %+v", accepted.Runtime)
		}
		if err := accepted.Runtime.Validate(); err != nil {
			t.Fatalf("the tagged pin was refused: %v", err)
		}
		for _, tc := range []struct{ name, block, field string }{
			{"no checksum", "runtime:\n  module: stego.dev/runtime\n  version: v0.4.1\n", "runtime.sum"},
			{"a branch", "runtime:\n  module: stego.dev/runtime\n  version: main\n  sum: " + sum + "\n", "runtime.version"},
			{"a sum that is no go.sum hash", "runtime:\n  module: stego.dev/runtime\n  version: v0.4.1\n  sum: sha256:abc\n", "runtime.sum"},
			{"a pseudo-version", "runtime:\n  module: stego.dev/runtime\n  version: v0.0.0-20260101000000-abcdefabcdef\n  sum: " + sum + "\n", "runtime.version"},
			{"a replace directive", "runtime:\n  module: stego.dev/runtime\n  version: v0.4.1\n  sum: " + sum + "\n  replace: ../runtime\n", "replace"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				archetype, err := registry.ParseArchetypeFromBytes([]byte(pinArchetype(tc.block)), "archetype.yaml")
				if err == nil {
					err = archetype.Runtime.Validate()
				}
				if err == nil || !strings.Contains(err.Error(), tc.field) {
					t.Fatalf("the pin with %s was not refused naming %s: %v", tc.name, tc.field, err)
				}
			})
		}
	})
}

// runtimeFolder is the runtime library of the common registry in this checkout.
func runtimeFolder(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "registry", "runtime")
}

func TestRuntimeReleasedWithTheCommonRegistry(t *testing.T) {
	t.Run("ST-STD-RUNTIME-R04-T01 Every archetype of the common registry pins the one runtime module of the registry, at the checksum of its folder", func(t *testing.T) {
		common, err := registry.Load(filepath.Join(repoRoot(t), "registry"))
		if err != nil {
			t.Fatal(err)
		}
		folder := common.RuntimeFolder()
		if folder == "" {
			t.Fatal("the common registry has no runtime folder")
		}
		if filepath.Clean(folder) != filepath.Clean(runtimeFolder(t)) {
			t.Fatalf("runtime folder = %s, want %s", folder, runtimeFolder(t))
		}
		data, err := os.ReadFile(filepath.Join(folder, "go.mod"))
		if err != nil {
			t.Fatal(err)
		}
		module := modfile.ModulePath(data)
		if module != registry.RuntimeModule {
			t.Fatalf("registry/runtime/go.mod names %q, want %q", module, registry.RuntimeModule)
		}
		// The proxy adds the repository's LICENSE to a module in a subfolder that has none, so the folder holds its
		// own copy, and the local zip is the same as the one the proxy makes from the tag.
		license, err := os.ReadFile(filepath.Join(folder, "LICENSE"))
		if err != nil {
			t.Fatalf("registry/runtime has no LICENSE of its own: %v", err)
		}
		root, err := os.ReadFile(filepath.Join(repoRoot(t), "LICENSE"))
		if err != nil || string(root) != string(license) {
			t.Fatalf("registry/runtime/LICENSE differs from the repository's LICENSE: %v", err)
		}
		var pins []registry.RuntimePin
		for name, archetype := range common.Archetypes() {
			if archetype.Runtime == nil {
				t.Fatalf("archetype %s pins no runtime library", name)
			}
			pins = append(pins, *archetype.Runtime)
		}
		if len(pins) == 0 {
			t.Fatal("the common registry has no archetype")
		}
		for _, pin := range pins[1:] {
			if pin != pins[0] {
				t.Fatalf("the archetypes pin %+v and %+v; one release of the common registry has one runtime version", pins[0], pin)
			}
		}
		sum, err := registry.RuntimeSum(folder, module, pins[0].Version, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if pins[0].Module != module || sum != pins[0].Sum {
			t.Fatalf("the archetypes pin %s %s %s, but registry/runtime is %s with the sum %s at that version. A runtime change is a new runtime version: set runtime.version and runtime.sum of every archetype in registry/archetypes, and give each archetype a new version (ST-SUP-INV10)", pins[0].Module, pins[0].Version, pins[0].Sum, module, sum)
		}
	})
}
