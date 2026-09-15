package compiler

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
)

type routeProbe struct {
	declared []gen.HTTPRoute
	wiring   *gen.Wiring
	renders  int
	change   func()
}

func (p *routeProbe) HTTPRoutes(gen.Context) ([]gen.HTTPRoute, error) { return p.declared, nil }
func (p *routeProbe) Generate(gen.Context) ([]gen.File, *gen.Wiring, error) {
	p.renders++
	if p.change != nil {
		p.change()
	}
	return nil, p.wiring, nil
}

func TestHTTPCompositionRejectsConflictsBeforeAnyGeneratorRuns(t *testing.T) {
	input := snapshotTestInput(t)
	first := &routeProbe{declared: []gen.HTTPRoute{{Pattern: "GET /livez", Discovery: true}}}
	last := &routeProbe{declared: []gen.HTTPRoute{{Pattern: "GET /livez", Discovery: true}}}
	input.Generators["stub-api"], input.Generators["stub-store"] = first, last
	result, err := Validate(input)
	if err != nil || !result.HasErrors() || !strings.Contains(FormatValidation(result), "route") {
		t.Fatal("conflicting public routes passed validation", result, err)
	}
	if plan, err := Reconcile(input); plan != nil || err == nil {
		t.Fatal("conflicting public routes produced a plan", plan, err)
	}
	if first.renders != 0 || last.renders != 0 {
		t.Fatal("a generator ran before route validation finished")
	}
}

func TestHTTPCompositionRejectsChangedGeneratorDeclarations(t *testing.T) {
	input := snapshotTestInput(t)
	probe := &routeProbe{
		declared: []gen.HTTPRoute{{Pattern: "GET /before"}},
		wiring:   &gen.Wiring{Routes: []string{`mux.HandleFunc("GET /after", http.NotFound)`}},
	}
	probe.change = func() { probe.declared[0].Pattern = "GET /after" }
	input.Generators["stub-api"] = probe
	if plan, err := Reconcile(input); plan != nil || err == nil || !strings.Contains(err.Error(), "differ from its preflight declaration") {
		t.Fatal("changed route declaration produced a plan", plan, err)
	}
}

func TestHTTPCompositionMatchesGeneratedDeclarations(t *testing.T) {
	input := snapshotTestInput(t)
	probe := &routeProbe{
		declared: []gen.HTTPRoute{{Pattern: "GET /safe"}},
		wiring:   &gen.Wiring{Routes: []string{`mux.HandleFunc("GET /safe", http.NotFound)`}},
	}
	input.Generators["stub-api"] = probe
	if _, err := Reconcile(input); err != nil {
		t.Fatal("matching route declaration was rejected", err)
	}
}

func TestHTTPCompositionRejectsDuplicateWiringBeforeAssembly(t *testing.T) {
	input := AssemblerInput{ModuleName: "example.com/routes", GoVersion: "1.26.8"}
	for i := 0; i < 2; i++ {
		input.Wirings = append(input.Wirings, ComponentWiring{Name: fmt.Sprint(i), Wiring: &gen.Wiring{DiscoveryRoutes: []string{`topMux.HandleFunc("GET /livez", http.NotFound)`}}})
	}
	if files, err := Assemble(input); len(files) != 0 || err == nil || !strings.Contains(err.Error(), "route") {
		t.Fatal("duplicate public wiring produced output", err)
	}
}
