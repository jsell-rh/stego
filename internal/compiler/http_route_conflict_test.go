package compiler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
)

// routeProvider is a stub generator that declares HTTP routes before it
// renders, as a component with routes does.
type routeProvider struct {
	stubGenerator
	routes []gen.HTTPRoute
}

func (p *routeProvider) HTTPRoutes(gen.Context) ([]gen.HTTPRoute, error) { return p.routes, nil }

func TestHTTPRoutesOfAllComponentsDoNotConflict(t *testing.T) {
	t.Run("ST-GEN-INV28-T Conflicting HTTP routes of two components stop the run", func(t *testing.T) {
		for name, groups := range map[string]map[string][]gen.HTTPRoute{
			"one pattern twice on the protected multiplexer": {"first": {{Pattern: "GET /items"}}, "second": {{Pattern: "GET /items"}}},
			"a public route on the protected fallback":       {"first": {{Pattern: "GET /items"}}, "second": {{Pattern: "/", Discovery: true}}},
			"a pattern that is not valid":                    {"first": {{Pattern: "GET /items"}}, "second": {{Pattern: "GET /items/{id"}}},
		} {
			err := gen.ValidateHTTPRouteGroups(groups)
			if err == nil || !strings.Contains(err.Error(), "second") {
				t.Errorf("%s: the groups were not refused with the component named: %v", name, err)
			}
		}
		if err := gen.ValidateHTTPRouteGroups(map[string][]gen.HTTPRoute{
			"application": {{Pattern: "/"}},
			"health":      {{Pattern: "GET /livez", Discovery: true}},
		}); err != nil {
			t.Fatalf("protected and public routes that do not conflict were refused: %v", err)
		}

		input := snapshotTestInput(t)
		input.Generators["stub-api"] = &routeProvider{stubGenerator: stubGenerator{wiring: &gen.Wiring{}}, routes: []gen.HTTPRoute{{Pattern: "GET /items"}}}
		input.Generators["stub-store"] = &routeProvider{stubGenerator: stubGenerator{wiring: &gen.Wiring{}}, routes: []gen.HTTPRoute{{Pattern: "GET /items"}}}
		plan, err := Reconcile(input)
		if plan != nil || err == nil || !strings.Contains(err.Error(), "route") {
			t.Fatalf("a plan of two components with one route went on: %v", err)
		}
		for _, name := range []string{"out", "go.mod", ".stego"} {
			if _, err := os.Stat(filepath.Join(input.ProjectDir, name)); !os.IsNotExist(err) {
				t.Fatalf("a refused plan wrote %s: %v", name, err)
			}
		}
	})
}
