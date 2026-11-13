package registry_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/registry"
)

func TestMixinAliasRegistry(t *testing.T) {
	t.Run("ST-SUP-UC10-R04-T04 A registry refuses an alias of an alias, an alias of a mixin it does not hold, and an alias that adds anything", func(t *testing.T) {
		write := writeMixin
		plain := func(name string) string {
			return "kind: mixin\nname: " + name + "\nversion: 1.0.0\nadds_components: []\nadds_slots: []\noverrides: none\n"
		}
		alias := func(name string, targets ...string) string {
			return "kind: mixin\nname: " + name + "\nversion: 1.0.0\ndeprecated_on: 2026-08-31\nsunset: 2027-02-28\nalias_of: [" + strings.Join(targets, ", ") + "]\nadds_components: []\nadds_slots: []\noverrides: none\n"
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
			writePolicy(t, dir, 6)
			for name, yaml := range c.files {
				write(t, dir, name, yaml)
			}
			if _, err := registry.Load(dir); err == nil || !strings.Contains(err.Error(), c.fault) {
				t.Errorf("%s: Load gave %v, want a refusal with %q", c.name, err, c.fault)
			}
		}
		// An alias of mixins that are no aliases loads.
		dir := t.TempDir()
		writePolicy(t, dir, 6)
		write(t, dir, "a", alias("a", "b", "c"))
		write(t, dir, "b", plain("b"))
		write(t, dir, "c", plain("c"))
		if reg, err := registry.Load(dir); err != nil || len(reg.Mixin("a").AliasOf) != 2 {
			t.Fatalf("Load of a plain alias gave %v", err)
		}
	})

	t.Run("ST-SUP-UC10-R04-T05 A registry holds a deprecated alias to its sunset and to the slot contracts of its mixins", func(t *testing.T) {
		const contract = "syntax = \"proto3\";\npackage p.slots;\nmessage Changed { string id = 1; }\n"
		target := "kind: mixin\nname: store-m\nversion: 1.0.0\nadds_components: []\nadds_slots:\n  - name: changed\n    proto: p.slots.Changed\n    default: noop\noverrides: none\n"
		alias := func(dates string) string {
			return "kind: mixin\nname: pub\nversion: 1.0.0\n" + dates + "alias_of: [store-m]\nadds_components: []\nadds_slots: []\noverrides: none\n"
		}
		const dates = "deprecated_on: 2026-08-31\nsunset: 2027-02-28\n"
		load := func(t *testing.T, ga int, pub, copy, plain string) (*registry.Registry, error) {
			t.Helper()
			dir := t.TempDir()
			if ga >= 0 {
				writePolicy(t, dir, ga)
			}
			writeMixin(t, dir, "store-m", target)
			writeFile(t, filepath.Join(dir, "mixins", "store-m", "slots", "changed.proto"), contract)
			writeMixin(t, dir, "pub", pub)
			if copy != "" {
				writeFile(t, filepath.Join(dir, "mixins", "pub", "slots", "changed.proto"), copy)
			}
			if plain != "" {
				writeMixin(t, dir, "other", plain)
			}
			return registry.Load(dir)
		}
		for _, c := range []struct {
			name, pub, copy, plain string
			ga                     int
			fault                  string
		}{
			{"no policy file", alias(dates), contract, "", -1, "registry policy file policy.yaml"},
			{"no sunset", alias("deprecated_on: 2026-08-31\n"), contract, "", 6, "states no sunset date"},
			{"no deprecated_on", alias("sunset: 2027-02-28\n"), contract, "", 6, "states no deprecated_on date"},
			{"a sunset that is not one ga window later", alias("deprecated_on: 2026-08-31\nsunset: 2027-03-01\n"), contract, "", 6, "plus the ga window of 6 months: 2027-02-28"},
			{"a sunset of another ga window", alias(dates), contract, "", 3, "plus the ga window of 3 months: 2026-11-30"},
			{"no copy of the slot contract", alias(dates), "", "", 6, "keeps no copy of the slot contract mixins/store-m/slots/changed.proto"},
			{"a copy that differs", alias(dates), contract + "// other\n", "", 6, "keeps no copy of the slot contract mixins/store-m/slots/changed.proto"},
			{"dates on a mixin that is no alias", alias(dates), contract, "kind: mixin\nname: other\nversion: 1.0.0\n" + dates + "adds_components: []\nadds_slots: []\noverrides: none\n", 6, "only a deprecated alias states them"},
		} {
			if _, err := load(t, c.ga, c.pub, c.copy, c.plain); err == nil || !strings.Contains(err.Error(), c.fault) {
				t.Errorf("%s: Load gave %v, want a refusal with %q", c.name, err, c.fault)
			}
		}
		reg, err := load(t, 6, alias(dates), contract, "")
		if err != nil {
			t.Fatalf("Load of an alias with its sunset and its copy of the contract: %v", err)
		}
		if got := reg.Mixin("pub").Sunset; got != "2027-02-28" {
			t.Errorf("the sunset of pub is %q, want 2027-02-28", got)
		}
	})
}

// writeMixin writes the mixin.yaml of a mixin into a test registry.
func writeMixin(t *testing.T, dir, name, yaml string) {
	t.Helper()
	writeFile(t, filepath.Join(dir, "mixins", name, "mixin.yaml"), yaml)
}

// writePolicy writes a registry policy file with a ga window of the months given.
func writePolicy(t *testing.T, dir string, ga int) {
	t.Helper()
	writeFile(t, filepath.Join(dir, "policy.yaml"), fmt.Sprintf("kind: policy\nwindows:\n  alpha: 0\n  beta: 3\n  ga: %d\n", ga))
}

// writeFile writes a file of a test registry, and its folders.
func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}
