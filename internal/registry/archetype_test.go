package registry_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/registry"
)

// repoRoot returns the absolute path to the repository root. It works by
// locating this source file at compile time and walking up to the repo root.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("unable to determine repo root via runtime.Caller")
	}
	// thisFile is internal/registry/archetype_test.go — go up 3 levels.
	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}

func TestRestCrudArchetypeParsesFromLiveRegistry(t *testing.T) {
	root := repoRoot(t)
	path := filepath.Join(root, "registry", "archetypes", "rest-crud", "archetype.yaml")

	a, err := registry.ParseArchetype(path)
	if err != nil {
		t.Fatalf("ParseArchetype() error: %v", err)
	}

	if a.Kind != "archetype" {
		t.Errorf("Kind = %q, want %q", a.Kind, "archetype")
	}
	if a.Name != "rest-crud" {
		t.Errorf("Name = %q, want %q", a.Name, "rest-crud")
	}
	if a.Language != "go" {
		t.Errorf("Language = %q, want %q", a.Language, "go")
	}
	if a.Version != "3.55.0" {
		t.Errorf("Version = %q, want %q", a.Version, "3.55.0")
	}
	if a.DefaultAuth != "jwt-auth" {
		t.Errorf("DefaultAuth = %q, want %q", a.DefaultAuth, "jwt-auth")
	}

	wantComponents := []string{"rest-api", "postgres-adapter", "domain-probe", "command", "query", "outbox", "otel-tracing", "health-check"}
	if len(a.Components) != len(wantComponents) {
		t.Fatalf("Components count = %d, want %d", len(a.Components), len(wantComponents))
	}
	for i, want := range wantComponents {
		if a.Components[i] != want {
			t.Errorf("Components[%d] = %q, want %q", i, a.Components[i], want)
		}
	}

	if a.Conventions.Layout != "flat" {
		t.Errorf("Conventions.Layout = %q, want %q", a.Conventions.Layout, "flat")
	}
	if a.Conventions.ErrorHandling != "problem-details-rfc" {
		t.Errorf("Conventions.ErrorHandling = %q, want %q", a.Conventions.ErrorHandling, "problem-details-rfc")
	}
	if a.Conventions.Logging != "structured-json" {
		t.Errorf("Conventions.Logging = %q, want %q", a.Conventions.Logging, "structured-json")
	}
	if a.Conventions.TestPattern != "table-driven" {
		t.Errorf("Conventions.TestPattern = %q, want %q", a.Conventions.TestPattern, "table-driven")
	}
	if a.Conventions.CORS != "enabled" {
		t.Errorf("Conventions.CORS = %q, want %q", a.Conventions.CORS, "enabled")
	}

	wantMixins := []string{"event-publisher", "async-worker"}
	if len(a.CompatibleMixins) != len(wantMixins) {
		t.Fatalf("CompatibleMixins count = %d, want %d", len(a.CompatibleMixins), len(wantMixins))
	}
	for i, want := range wantMixins {
		if a.CompatibleMixins[i] != want {
			t.Errorf("CompatibleMixins[%d] = %q, want %q", i, a.CompatibleMixins[i], want)
		}
	}

	if len(a.Bindings) != 3 {
		t.Fatalf("Bindings count = %d, want 3", len(a.Bindings))
	}
	if a.Bindings["durable-outbox"] != "outbox" {
		t.Errorf("Bindings[durable-outbox] = %q, want outbox", a.Bindings["durable-outbox"])
	}
	if a.Bindings["storage-adapter"] != "postgres-adapter" {
		t.Errorf("Bindings[storage-adapter] = %q, want %q", a.Bindings["storage-adapter"], "postgres-adapter")
	}
	if a.Bindings["auth-provider"] != "jwt-auth" {
		t.Errorf("Bindings[auth-provider] = %q, want %q", a.Bindings["auth-provider"], "jwt-auth")
	}
}

func TestLiveRegistryLoadsAllArchetypeComponents(t *testing.T) {
	root := repoRoot(t)
	regDir := filepath.Join(root, "registry")

	reg, err := registry.Load(regDir)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	a := reg.Archetype("rest-crud")
	if a == nil {
		t.Fatal("Archetype(rest-crud) returned nil")
	}

	// The live registry should have all four components referenced by the
	// archetype.
	restAPI := reg.Component("rest-api")
	if restAPI == nil {
		t.Fatal("Component(rest-api) returned nil")
	}
	if restAPI.Version != "4.1.1" {
		t.Errorf("rest-api Version = %q, want %q", restAPI.Version, "4.1.1")
	}

	pg := reg.Component("postgres-adapter")
	if pg == nil {
		t.Fatal("Component(postgres-adapter) returned nil")
	}
	if pg.Version != "6.0.1" {
		t.Errorf("postgres-adapter Version = %q, want %q", pg.Version, "6.0.1")
	}

	otel := reg.Component("otel-tracing")
	if otel == nil {
		t.Fatal("Component(otel-tracing) returned nil")
	}
	if len(otel.Provides) != 1 || otel.Provides[0].Name != "tracing" {
		t.Errorf("otel-tracing Provides unexpected: %+v", otel.Provides)
	}

	hc := reg.Component("health-check")
	if hc == nil {
		t.Fatal("Component(health-check) returned nil")
	}
	if len(hc.Provides) != 1 || hc.Provides[0].Name != "health-endpoint" {
		t.Errorf("health-check Provides unexpected: %+v", hc.Provides)
	}

	// Every component referenced by the archetype must be present in the registry.
	for _, compName := range a.Components {
		if reg.Component(compName) == nil {
			t.Errorf("archetype references component %q but registry does not contain it", compName)
		}
	}
}

func TestShippedGoldenPathsHoldOnlyGAComponents(t *testing.T) {
	t.Run("ST-SUP-SP01-T Every archetype and mixin of the shipped registry includes only ga components", func(t *testing.T) {
		shipped, err := registry.Load(filepath.Join(repoRoot(t), "registry"))
		if err != nil {
			t.Fatal(err)
		}
		experiments, evaluated := 0, 0
		evaluate := func(owner string, components []string) {
			for _, name := range components {
				component := shipped.Component(name)
				if component == nil {
					t.Fatalf("%s includes %s, which the registry does not hold", owner, name)
				}
				evaluated++
				if !registry.FitForGoldenPath(component) {
					experiments++
					t.Errorf("%s includes %s at stage %q", owner, name, component.Stage)
				}
			}
		}
		for name, archetype := range shipped.Archetypes() {
			evaluate("archetype "+name, archetype.Components)
		}
		for name, mixin := range shipped.Mixins() {
			evaluate("mixin "+name, mixin.AddsComponents)
		}
		if evaluated == 0 || experiments != 0 {
			t.Fatalf("%d experiments on golden paths among %d included components, want 0 of more than 0", experiments, evaluated)
		}

		fixture := t.TempDir()
		for name, data := range map[string]string{
			"components/store-a/component.yaml":  "kind: component\nname: store-a\nversion: 1.0.0\nstage: ga\nrequires: []\nprovides: []\nslots: []\n",
			"components/beta-x/component.yaml":   "kind: component\nname: beta-x\nversion: 0.2.0\nstage: beta\nsunset: 2099-01-01\nrequires: []\nprovides: []\nslots: []\n",
			"archetypes/svc-arch/archetype.yaml": "kind: archetype\nruntime: {module: github.com/jsell-rh/stego/registry/runtime, version: v0.1.0, sum: 'h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA='}\nname: svc-arch\nlanguage: go\nversion: 1.0.0\ncomponents: [store-a, beta-x]\n",
		} {
			target := filepath.Join(fixture, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(target, []byte(data), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := registry.Load(fixture); err == nil || !strings.Contains(err.Error(), "svc-arch") || !strings.Contains(err.Error(), "beta-x (beta)") || strings.Contains(err.Error(), "store-a") {
			t.Fatalf("the archetype with a beta component was not refused naming beta-x and its stage only: %v", err)
		}
	})
}
