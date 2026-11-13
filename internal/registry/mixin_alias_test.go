package registry_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/registry"
)

func TestMixinAliasRegistry(t *testing.T) {
	t.Run("ST-SUP-UC10-R04-T04 A registry refuses an alias of an alias, an alias of a mixin it does not hold, and an alias that adds anything", func(t *testing.T) {
		write := func(t *testing.T, dir, name, yaml string) {
			t.Helper()
			folder := filepath.Join(dir, "mixins", name)
			if err := os.MkdirAll(folder, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(folder, "mixin.yaml"), []byte(yaml), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		plain := func(name string) string {
			return "kind: mixin\nname: " + name + "\nversion: 1.0.0\nadds_components: []\nadds_slots: []\noverrides: none\n"
		}
		alias := func(name string, targets ...string) string {
			return "kind: mixin\nname: " + name + "\nversion: 1.0.0\nalias_of: [" + strings.Join(targets, ", ") + "]\nadds_components: []\nadds_slots: []\noverrides: none\n"
		}
		for _, c := range []struct {
			name  string
			files map[string]string
			fault string
		}{
			{"an alias of an alias", map[string]string{"a": alias("a", "b"), "b": alias("b", "c"), "c": plain("c")}, "is an alias too"},
			{"an alias of itself", map[string]string{"a": alias("a", "a")}, "is an alias too"},
			{"an alias of a mixin the registry does not hold", map[string]string{"a": alias("a", "missing")}, "which the registry does not hold"},
			{"an alias that adds a component", map[string]string{"a": strings.Replace(alias("a", "c"), "adds_components: []", "adds_components: [x]", 1), "c": plain("c")}, "adds or requires nothing of its own"},
		} {
			dir := t.TempDir()
			for name, yaml := range c.files {
				write(t, dir, name, yaml)
			}
			if _, err := registry.Load(dir); err == nil || !strings.Contains(err.Error(), c.fault) {
				t.Errorf("%s: Load gave %v, want a refusal with %q", c.name, err, c.fault)
			}
		}
		// An alias of mixins that are no aliases loads.
		dir := t.TempDir()
		write(t, dir, "a", alias("a", "b", "c"))
		write(t, dir, "b", plain("b"))
		write(t, dir, "c", plain("c"))
		if reg, err := registry.Load(dir); err != nil || len(reg.Mixin("a").AliasOf) != 2 {
			t.Fatalf("Load of a plain alias gave %v", err)
		}
	})
}
