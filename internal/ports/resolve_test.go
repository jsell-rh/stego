package ports

import (
	"errors"
	"strings"
	"testing"

	"github.com/stego-project/stego/internal/types"
)

// restCrudComponents returns the component set matching the spec's rest-crud archetype.
func restCrudComponents() map[string]*types.Component {
	return map[string]*types.Component{
		"rest-api": {
			Name:    "rest-api",
			Version: "2.1.0",
			Requires: []types.Port{
				{Name: "auth-provider"},
				{Name: "storage-adapter"},
			},
			Provides: []types.Port{
				{Name: "http-server"},
				{Name: "openapi-spec"},
			},
		},
		"postgres-adapter": {
			Name:    "postgres-adapter",
			Version: "1.4.0",
			Provides: []types.Port{
				{Name: "storage-adapter"},
			},
		},
		"jwt-auth": {
			Name:    "jwt-auth",
			Version: "1.0.0",
			Provides: []types.Port{
				{Name: "auth-provider"},
			},
		},
		"otel-tracing": {
			Name:    "otel-tracing",
			Version: "1.0.0",
		},
		"health-check": {
			Name:    "health-check",
			Version: "1.0.0",
		},
	}
}

func TestResolveRestCrudDefaults(t *testing.T) {
	result, err := Resolve(ResolveInput{
		Components: restCrudComponents(),
		ArchetypeBindings: map[string]string{
			"storage-adapter": "postgres-adapter",
			"auth-provider":   "jwt-auth",
		},
	})
	if err != nil {
		t.Fatalf("Resolve() error: %v", err)
	}

	restBindings, ok := result.Bindings["rest-api"]
	if !ok {
		t.Fatal("expected bindings for rest-api")
	}
	if restBindings["storage-adapter"] != "postgres-adapter" {
		t.Errorf("rest-api storage-adapter = %q, want %q", restBindings["storage-adapter"], "postgres-adapter")
	}
	if restBindings["auth-provider"] != "jwt-auth" {
		t.Errorf("rest-api auth-provider = %q, want %q", restBindings["auth-provider"], "jwt-auth")
	}

	// Components without requires should have no bindings entry.
	if _, ok := result.Bindings["postgres-adapter"]; ok {
		t.Error("expected no bindings for postgres-adapter (no requires)")
	}
	if _, ok := result.Bindings["otel-tracing"]; ok {
		t.Error("expected no bindings for otel-tracing (no requires)")
	}
}

func TestResolveServiceOverrideTakesPrecedence(t *testing.T) {
	components := restCrudComponents()
	components["api-key-auth"] = &types.Component{
		Name:    "api-key-auth",
		Version: "1.0.0",
		Provides: []types.Port{
			{Name: "auth-provider"},
		},
	}

	result, err := Resolve(ResolveInput{
		Components: components,
		ArchetypeBindings: map[string]string{
			"storage-adapter": "postgres-adapter",
			"auth-provider":   "jwt-auth",
		},
		ServiceOverrides: map[string]string{
			"auth-provider": "api-key-auth",
		},
	})
	if err != nil {
		t.Fatalf("Resolve() error: %v", err)
	}

	restBindings := result.Bindings["rest-api"]
	if restBindings["auth-provider"] != "api-key-auth" {
		t.Errorf("rest-api auth-provider = %q, want %q (service override should take precedence)",
			restBindings["auth-provider"], "api-key-auth")
	}
	if restBindings["storage-adapter"] != "postgres-adapter" {
		t.Errorf("rest-api storage-adapter = %q, want %q", restBindings["storage-adapter"], "postgres-adapter")
	}
}

func TestResolveAutoDiscovery(t *testing.T) {
	// When no explicit binding exists but exactly one component provides
	// the port, it should be auto-discovered.
	result, err := Resolve(ResolveInput{
		Components: restCrudComponents(),
		// No explicit bindings -- rely on auto-discovery.
		ArchetypeBindings: map[string]string{},
	})
	if err != nil {
		t.Fatalf("Resolve() error: %v", err)
	}

	restBindings := result.Bindings["rest-api"]
	if restBindings["storage-adapter"] != "postgres-adapter" {
		t.Errorf("auto-discovered storage-adapter = %q, want %q", restBindings["storage-adapter"], "postgres-adapter")
	}
	if restBindings["auth-provider"] != "jwt-auth" {
		t.Errorf("auto-discovered auth-provider = %q, want %q", restBindings["auth-provider"], "jwt-auth")
	}
}

func TestResolveUnresolvedPort(t *testing.T) {
	// Remove the component that provides storage-adapter.
	components := restCrudComponents()
	delete(components, "postgres-adapter")

	_, err := Resolve(ResolveInput{
		Components:        components,
		ArchetypeBindings: map[string]string{},
	})
	if err == nil {
		t.Fatal("expected error for unresolved port, got nil")
	}

	var resErr *ResolutionError
	if !errors.As(err, &resErr) {
		t.Fatalf("expected *ResolutionError, got %T: %v", err, err)
	}
	if len(resErr.Unresolved) == 0 {
		t.Fatal("expected at least one unresolved port")
	}

	found := false
	for _, u := range resErr.Unresolved {
		if u.Port == "storage-adapter" && u.Component == "rest-api" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected unresolved port storage-adapter for rest-api, got: %+v", resErr.Unresolved)
	}

	if !strings.Contains(err.Error(), "unresolved port") {
		t.Errorf("error message should contain 'unresolved port', got: %v", err)
	}
}

func TestResolveAmbiguousPort(t *testing.T) {
	components := restCrudComponents()
	// Add a second component that also provides storage-adapter.
	components["sqlite-adapter"] = &types.Component{
		Name:    "sqlite-adapter",
		Version: "1.0.0",
		Provides: []types.Port{
			{Name: "storage-adapter"},
		},
	}

	_, err := Resolve(ResolveInput{
		Components:        components,
		ArchetypeBindings: map[string]string{},
	})
	if err == nil {
		t.Fatal("expected error for ambiguous port, got nil")
	}

	var resErr *ResolutionError
	if !errors.As(err, &resErr) {
		t.Fatalf("expected *ResolutionError, got %T: %v", err, err)
	}
	if len(resErr.Ambiguous) == 0 {
		t.Fatal("expected at least one ambiguous port")
	}

	found := false
	for _, a := range resErr.Ambiguous {
		if a.Port == "storage-adapter" && a.Component == "rest-api" {
			found = true
			if len(a.Providers) != 2 {
				t.Errorf("expected 2 providers, got %d: %v", len(a.Providers), a.Providers)
			}
		}
	}
	if !found {
		t.Errorf("expected ambiguous port storage-adapter for rest-api, got: %+v", resErr.Ambiguous)
	}

	if !strings.Contains(err.Error(), "ambiguous port") {
		t.Errorf("error message should contain 'ambiguous port', got: %v", err)
	}
}

func TestResolveAmbiguousResolvedByBinding(t *testing.T) {
	// Two providers for storage-adapter, but archetype binding disambiguates.
	components := restCrudComponents()
	components["sqlite-adapter"] = &types.Component{
		Name:    "sqlite-adapter",
		Version: "1.0.0",
		Provides: []types.Port{
			{Name: "storage-adapter"},
		},
	}

	result, err := Resolve(ResolveInput{
		Components: components,
		ArchetypeBindings: map[string]string{
			"storage-adapter": "postgres-adapter",
			"auth-provider":   "jwt-auth",
		},
	})
	if err != nil {
		t.Fatalf("Resolve() error: %v", err)
	}

	restBindings := result.Bindings["rest-api"]
	if restBindings["storage-adapter"] != "postgres-adapter" {
		t.Errorf("storage-adapter = %q, want %q", restBindings["storage-adapter"], "postgres-adapter")
	}
}

func TestResolveBindingToNonExistentComponent(t *testing.T) {
	// Binding references a component that doesn't exist.
	_, err := Resolve(ResolveInput{
		Components: restCrudComponents(),
		ArchetypeBindings: map[string]string{
			"storage-adapter": "nonexistent-adapter",
			"auth-provider":   "jwt-auth",
		},
	})
	if err == nil {
		t.Fatal("expected error for binding to non-existent component, got nil")
	}

	var resErr *ResolutionError
	if !errors.As(err, &resErr) {
		t.Fatalf("expected *ResolutionError, got %T: %v", err, err)
	}
	if len(resErr.Unresolved) == 0 {
		t.Fatal("expected unresolved port for invalid binding")
	}
}

func TestResolveBindingToComponentThatDoesNotProvidePort(t *testing.T) {
	// Binding maps storage-adapter to otel-tracing, which doesn't provide it.
	_, err := Resolve(ResolveInput{
		Components: restCrudComponents(),
		ArchetypeBindings: map[string]string{
			"storage-adapter": "otel-tracing",
			"auth-provider":   "jwt-auth",
		},
	})
	if err == nil {
		t.Fatal("expected error for binding to component that doesn't provide port, got nil")
	}

	var resErr *ResolutionError
	if !errors.As(err, &resErr) {
		t.Fatalf("expected *ResolutionError, got %T: %v", err, err)
	}
	if len(resErr.Unresolved) == 0 {
		t.Fatal("expected unresolved port for invalid binding target")
	}
}

func TestResolveNoRequires(t *testing.T) {
	// All components have no requires -- should succeed with empty bindings.
	components := map[string]*types.Component{
		"health-check": {
			Name:    "health-check",
			Version: "1.0.0",
		},
	}

	result, err := Resolve(ResolveInput{
		Components: components,
	})
	if err != nil {
		t.Fatalf("Resolve() error: %v", err)
	}

	if len(result.Bindings) != 0 {
		t.Errorf("expected empty bindings, got %v", result.Bindings)
	}
}

func TestResolveMultipleUnresolvedAndAmbiguous(t *testing.T) {
	// Tests that all errors are reported, not just the first one.
	components := map[string]*types.Component{
		"comp-a": {
			Name: "comp-a",
			Requires: []types.Port{
				{Name: "port-missing"},
				{Name: "port-ambiguous"},
			},
		},
		"provider-1": {
			Name:     "provider-1",
			Provides: []types.Port{{Name: "port-ambiguous"}},
		},
		"provider-2": {
			Name:     "provider-2",
			Provides: []types.Port{{Name: "port-ambiguous"}},
		},
	}

	_, err := Resolve(ResolveInput{
		Components: components,
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var resErr *ResolutionError
	if !errors.As(err, &resErr) {
		t.Fatalf("expected *ResolutionError, got %T", err)
	}
	if len(resErr.Unresolved) != 1 {
		t.Errorf("expected 1 unresolved, got %d", len(resErr.Unresolved))
	}
	if len(resErr.Ambiguous) != 1 {
		t.Errorf("expected 1 ambiguous, got %d", len(resErr.Ambiguous))
	}

	errMsg := err.Error()
	if !strings.Contains(errMsg, "unresolved port") || !strings.Contains(errMsg, "ambiguous port") {
		t.Errorf("error should mention both unresolved and ambiguous, got: %v", errMsg)
	}
}
