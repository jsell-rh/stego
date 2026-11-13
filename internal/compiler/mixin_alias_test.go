package compiler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// aliasRegistry adds to the registry of a test input the mixins store-m, send-m, which requires store-m, and the
// alias pub of both, deprecated with a ga window of 6 months, and gives the archetype the compatible mixins named.
func aliasRegistry(t *testing.T, input ReconcilerInput, compatible ...string) {
	t.Helper()
	for name, yaml := range map[string]string{
		"store-m": "kind: mixin\nname: store-m\nversion: 1.0.0\nadds_components: []\nadds_slots: []\noverrides: none\n",
		"send-m":  "kind: mixin\nname: send-m\nversion: 1.0.0\nrequires_mixins: [store-m]\nadds_components: []\nadds_slots: []\noverrides: none\n",
		"pub":     "kind: mixin\nname: pub\nversion: 1.0.0\ndeprecated_on: 2026-11-13\nsunset: 2027-05-13\nalias_of: [store-m, send-m]\nadds_components: []\nadds_slots: []\noverrides: none\n",
	} {
		if err := os.MkdirAll(filepath.Join(input.RegistryDir, "mixins", name), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(input.RegistryDir, "mixins", name, "mixin.yaml"), yaml)
	}
	writeFile(t, filepath.Join(input.RegistryDir, "policy.yaml"), "kind: policy\nwindows:\n  ga: 6\n")
	path := filepath.Join(input.RegistryDir, "archetypes", "test-arch", "archetype.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, string(data)+"compatible_mixins: ["+strings.Join(compatible, ", ")+"]\n")
}

// withMixins gives the declaration of a test input the mixins named.
func withMixins(t *testing.T, input ReconcilerInput, mixins ...string) {
	t.Helper()
	path := filepath.Join(input.ProjectDir, "service.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, string(data)+"\nmixins: ["+strings.Join(mixins, ", ")+"]\n")
}

func TestMixinAliasExpansion(t *testing.T) {
	t.Run("ST-SUP-UC10-R04-T03 An alias binds only the mixins that the archetype allows, and a mixin needs the mixins it requires", func(t *testing.T) {
		// The alias is allowed, and one mixin it names is not: the expanded mixin is refused by name.
		input := snapshotTestInput(t)
		aliasRegistry(t, input, "pub", "store-m")
		withMixins(t, input, "pub")
		if plan, err := Reconcile(input); plan != nil || err == nil || !strings.Contains(err.Error(), `mixin "send-m" is not compatible`) {
			t.Fatalf("Reconcile of the alias with a mixin that the archetype does not allow gave %v, %v; want the refusal of send-m", plan, err)
		}
		// Every mixin that the alias names is allowed: the alias binds both.
		input = snapshotTestInput(t)
		aliasRegistry(t, input, "pub", "store-m", "send-m")
		withMixins(t, input, "pub")
		if _, err := Reconcile(input); err != nil {
			t.Fatalf("Reconcile of the alias of allowed mixins: %v", err)
		}
		// A mixin without a mixin it requires is refused, naming both.
		input = snapshotTestInput(t)
		aliasRegistry(t, input, "pub", "store-m", "send-m")
		withMixins(t, input, "send-m")
		if plan, err := Reconcile(input); plan != nil || err == nil || !strings.Contains(err.Error(), `mixin "send-m" requires the mixin "store-m"`) {
			t.Fatalf("Reconcile of send-m alone gave %v, %v; want the refusal that names send-m and store-m", plan, err)
		}
	})
}
