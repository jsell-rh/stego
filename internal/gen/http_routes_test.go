package gen

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestHTTPRouteGroupsRejectLegacyMuxMode(t *testing.T) {
	const marker = "STEGO_TEST_ROUTE_GROUP_LEGACY_MUX"
	if os.Getenv(marker) == "1" {
		err := ValidateHTTPRouteGroups(map[string][]HTTPRoute{"api": {{Pattern: "GET /items"}}})
		if err == nil || !strings.Contains(err.Error(), "pattern semantics") {
			t.Fatal("legacy mux mode bypassed route validation", err)
		}
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHTTPRouteGroupsRejectLegacyMuxMode$", "-test.count=1")
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GODEBUG=") && !strings.HasPrefix(entry, marker+"=") {
			command.Env = append(command.Env, entry)
		}
	}
	command.Env = append(command.Env, "GODEBUG=httpmuxgo121=1", marker+"=1")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("legacy mux check failed: %v\n%s", err, output)
	}
}

func TestHTTPRouteGroupsRejectRegistrationFailures(t *testing.T) {
	for name, groups := range map[string]map[string][]HTTPRoute{
		"public health collision":   {"metadata": {{Pattern: "GET /livez", Discovery: true}}, "health": {{Pattern: "GET /livez", Discovery: true}}},
		"protected duplicate":       {"first": {{Pattern: "GET /items"}}, "second": {{Pattern: "GET /items"}}},
		"wildcard conflict":         {"first": {{Pattern: "GET /{owner}/users"}}, "second": {{Pattern: "GET /owners/{user}"}}},
		"invalid pattern":           {"first": {{Pattern: "GET /items/{id"}}},
		"public fallback collision": {"first": {{Pattern: "/", Discovery: true}}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateHTTPRouteGroups(groups); err == nil || !strings.Contains(err.Error(), "route") {
				t.Fatal("invalid composition was accepted", err)
			}
		})
	}
}

func TestHTTPRouteGroupsPreserveProtectedMountAndPublicProbes(t *testing.T) {
	groups := map[string][]HTTPRoute{
		"application": {{Pattern: "/"}},
		"health":      {{Pattern: "GET /livez", Discovery: true}, {Pattern: "GET /readyz", Discovery: true}},
		"metadata":    {{Pattern: "GET /{$}", Discovery: true}},
	}
	if err := ValidateHTTPRouteGroups(groups); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPWiringRequiresLiteralRegistrationsOnTheDeclaredMux(t *testing.T) {
	for _, statement := range []string{
		`mux.HandleFunc(route, http.NotFound)`, `mux.HandleFunc("GET /items")`,
		`topMux.HandleFunc("GET /items", http.NotFound)`, `handler.Register(mux)`,
		`mux.HandleFunc("GET /items", http.NotFound`,
	} {
		t.Run(statement, func(t *testing.T) {
			if _, err := WiringHTTPRoutes(&Wiring{Routes: []string{statement}}); err == nil {
				t.Fatal("unsupported route expression was accepted")
			}
		})
	}
	actual, err := WiringHTTPRoutes(&Wiring{Routes: []string{`mux.Handle("/", handler)`}, DiscoveryRoutes: []string{`topMux.HandleFunc("GET /livez", monitor.Live)`}})
	if err != nil || len(actual) != 2 || actual[0] != (HTTPRoute{Pattern: "/"}) || actual[1] != (HTTPRoute{Pattern: "GET /livez", Discovery: true}) {
		t.Fatal("valid route expressions changed", actual, err)
	}
}
