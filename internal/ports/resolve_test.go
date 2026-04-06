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

func TestResolveNoBindingIsUnresolved(t *testing.T) {
	// Without explicit bindings, ports are unresolved — auto-discovery is not
	// a spec-defined resolution strategy.
	_, err := Resolve(ResolveInput{
		Components:        restCrudComponents(),
		ArchetypeBindings: map[string]string{},
	})
	if err == nil {
		t.Fatal("expected error for ports without bindings, got nil")
	}

	var resErr *ResolutionError
	if !errors.As(err, &resErr) {
		t.Fatalf("expected *ResolutionError, got %T: %v", err, err)
	}
	if len(resErr.Unresolved) != 2 {
		t.Fatalf("expected 2 unresolved ports, got %d: %+v", len(resErr.Unresolved), resErr.Unresolved)
	}

	ports := map[string]bool{}
	for _, u := range resErr.Unresolved {
		ports[u.Port] = true
		if u.Component != "rest-api" {
			t.Errorf("unexpected component %q for unresolved port", u.Component)
		}
	}
	if !ports["auth-provider"] || !ports["storage-adapter"] {
		t.Errorf("expected unresolved ports auth-provider and storage-adapter, got: %+v", resErr.Unresolved)
	}
}

func TestResolveUnresolvedPort(t *testing.T) {
	// Remove the component that provides storage-adapter, and don't bind it.
	components := restCrudComponents()
	delete(components, "postgres-adapter")

	_, err := Resolve(ResolveInput{
		Components: components,
		ArchetypeBindings: map[string]string{
			"auth-provider": "jwt-auth",
		},
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
	if len(resErr.InvalidBinding) != 1 {
		t.Fatalf("expected 1 invalid binding, got %d: %+v", len(resErr.InvalidBinding), resErr.InvalidBinding)
	}

	ib := resErr.InvalidBinding[0]
	if ib.Port != "storage-adapter" || ib.BoundTo != "nonexistent-adapter" {
		t.Errorf("unexpected invalid binding: %+v", ib)
	}
	if !strings.Contains(ib.Reason, "non-existent component") {
		t.Errorf("expected reason to mention non-existent component, got: %q", ib.Reason)
	}
	if !strings.Contains(err.Error(), "invalid binding") {
		t.Errorf("error message should contain 'invalid binding', got: %v", err)
	}
}

func TestResolveBindingToComponentThatDoesNotProvidePort(t *testing.T) {
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
	if len(resErr.InvalidBinding) != 1 {
		t.Fatalf("expected 1 invalid binding, got %d: %+v", len(resErr.InvalidBinding), resErr.InvalidBinding)
	}

	ib := resErr.InvalidBinding[0]
	if ib.Port != "storage-adapter" || ib.BoundTo != "otel-tracing" {
		t.Errorf("unexpected invalid binding: %+v", ib)
	}
	if !strings.Contains(ib.Reason, "does not provide port") {
		t.Errorf("expected reason to mention 'does not provide port', got: %q", ib.Reason)
	}
	if !strings.Contains(err.Error(), "invalid binding") {
		t.Errorf("error message should contain 'invalid binding', got: %v", err)
	}
}

func TestResolveNoRequires(t *testing.T) {
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

func TestResolveMultipleUnresolvedAndInvalidBinding(t *testing.T) {
	// Tests that all errors are reported, not just the first one.
	components := map[string]*types.Component{
		"comp-a": {
			Name: "comp-a",
			Requires: []types.Port{
				{Name: "port-missing"},
				{Name: "port-bad-binding"},
			},
		},
		"wrong-provider": {
			Name:     "wrong-provider",
			Provides: []types.Port{{Name: "something-else"}},
		},
	}

	_, err := Resolve(ResolveInput{
		Components: components,
		ArchetypeBindings: map[string]string{
			"port-bad-binding": "wrong-provider",
		},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var resErr *ResolutionError
	if !errors.As(err, &resErr) {
		t.Fatalf("expected *ResolutionError, got %T", err)
	}
	if len(resErr.Unresolved) != 1 {
		t.Errorf("expected 1 unresolved, got %d: %+v", len(resErr.Unresolved), resErr.Unresolved)
	}
	if len(resErr.InvalidBinding) != 1 {
		t.Errorf("expected 1 invalid binding, got %d: %+v", len(resErr.InvalidBinding), resErr.InvalidBinding)
	}

	errMsg := err.Error()
	if !strings.Contains(errMsg, "unresolved port") || !strings.Contains(errMsg, "invalid binding") {
		t.Errorf("error should mention both unresolved and invalid binding, got: %v", errMsg)
	}
}

func TestResolveSelfResolution(t *testing.T) {
	// A component that requires and provides the same port should not
	// resolve to itself.
	components := map[string]*types.Component{
		"self-ref": {
			Name: "self-ref",
			Requires: []types.Port{
				{Name: "some-port"},
			},
			Provides: []types.Port{
				{Name: "some-port"},
			},
		},
	}

	_, err := Resolve(ResolveInput{
		Components: components,
		ArchetypeBindings: map[string]string{
			"some-port": "self-ref",
		},
	})
	if err == nil {
		t.Fatal("expected error for self-resolution, got nil")
	}

	var resErr *ResolutionError
	if !errors.As(err, &resErr) {
		t.Fatalf("expected *ResolutionError, got %T: %v", err, err)
	}
	if len(resErr.InvalidBinding) != 1 {
		t.Fatalf("expected 1 invalid binding for self-resolution, got %d: %+v", len(resErr.InvalidBinding), resErr.InvalidBinding)
	}

	ib := resErr.InvalidBinding[0]
	if ib.Component != "self-ref" || ib.BoundTo != "self-ref" {
		t.Errorf("unexpected invalid binding: %+v", ib)
	}
	if !strings.Contains(ib.Reason, "cannot be bound to itself") {
		t.Errorf("expected reason to mention self-binding, got: %q", ib.Reason)
	}
}

func TestResolveMutualCycle(t *testing.T) {
	// Two components that each require what the other provides — valid as long
	// as explicit bindings exist. This is not self-resolution.
	components := map[string]*types.Component{
		"comp-a": {
			Name:     "comp-a",
			Requires: []types.Port{{Name: "port-b"}},
			Provides: []types.Port{{Name: "port-a"}},
		},
		"comp-b": {
			Name:     "comp-b",
			Requires: []types.Port{{Name: "port-a"}},
			Provides: []types.Port{{Name: "port-b"}},
		},
	}

	result, err := Resolve(ResolveInput{
		Components: components,
		ArchetypeBindings: map[string]string{
			"port-a": "comp-a",
			"port-b": "comp-b",
		},
	})
	if err != nil {
		t.Fatalf("Resolve() error: %v (mutual dependencies with explicit bindings should succeed)", err)
	}

	if result.Bindings["comp-a"]["port-b"] != "comp-b" {
		t.Errorf("comp-a port-b = %q, want %q", result.Bindings["comp-a"]["port-b"], "comp-b")
	}
	if result.Bindings["comp-b"]["port-a"] != "comp-a" {
		t.Errorf("comp-b port-a = %q, want %q", result.Bindings["comp-b"]["port-a"], "comp-a")
	}
}
