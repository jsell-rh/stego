package gen

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"sort"
	"strconv"
)

// HTTPRoute describes a registration on the protected or public multiplexer.
type HTTPRoute struct {
	Pattern   string
	Discovery bool
}

// HTTPRouteProvider declares routes before the compiler renders output.
// Generate must return the same routes, in the same order within each group.
type HTTPRouteProvider interface {
	HTTPRoutes(Context) ([]HTTPRoute, error)
}

func registerHTTPPattern(mux *http.ServeMux, pattern string) (err error) {
	defer func() {
		if failure := recover(); failure != nil {
			err = fmt.Errorf("invalid or conflicting HTTP route %q: %v", pattern, failure)
		}
	}()
	mux.HandleFunc(pattern, func(http.ResponseWriter, *http.Request) {})
	return nil
}

// ValidateHTTPRouteGroups checks the same multiplexer boundaries as main.go.
func ValidateHTTPRouteGroups(groups map[string][]HTTPRoute) error {
	var names []string
	for name, routes := range groups {
		if len(routes) > 0 {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	if registerHTTPPattern(http.NewServeMux(), "GET /{") == nil {
		return fmt.Errorf("HTTP route validation requires Go 1.22 pattern semantics")
	}
	sort.Strings(names)
	protected, public := http.NewServeMux(), http.NewServeMux()
	hasPublic := false
	for _, name := range names {
		for _, route := range groups[name] {
			mux := protected
			if route.Discovery {
				mux, hasPublic = public, true
			}
			if err := registerHTTPPattern(mux, route.Pattern); err != nil {
				return fmt.Errorf("component %q: %w", name, err)
			}
		}
	}
	if hasPublic {
		if err := registerHTTPPattern(public, "/"); err != nil {
			return fmt.Errorf("public routes conflict with the protected fallback: %w", err)
		}
	}
	return nil
}

// WiringHTTPRoutes reads literal registrations without running handler code.
func WiringHTTPRoutes(wiring *Wiring) ([]HTTPRoute, error) {
	if wiring == nil {
		return nil, nil
	}
	var routes []HTTPRoute
	for _, group := range []struct {
		statements []string
		receiver   string
		discovery  bool
	}{{wiring.Routes, "mux", false}, {wiring.DiscoveryRoutes, "topMux", true}} {
		for _, statement := range group.statements {
			expression, err := parser.ParseExpr(statement)
			if err != nil {
				return nil, fmt.Errorf("invalid HTTP route expression: %w", err)
			}
			call, ok := expression.(*ast.CallExpr)
			if !ok || len(call.Args) != 2 {
				return nil, fmt.Errorf("HTTP routes require a registration with two arguments")
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (selector.Sel.Name != "Handle" && selector.Sel.Name != "HandleFunc") {
				return nil, fmt.Errorf("HTTP routes require Handle or HandleFunc")
			}
			receiver, ok := selector.X.(*ast.Ident)
			if !ok || receiver.Name != group.receiver {
				return nil, fmt.Errorf("HTTP route must register on %s", group.receiver)
			}
			literal, ok := call.Args[0].(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return nil, fmt.Errorf("HTTP route pattern must be a string literal")
			}
			pattern, err := strconv.Unquote(literal.Value)
			if err != nil {
				return nil, fmt.Errorf("invalid HTTP route pattern: %w", err)
			}
			routes = append(routes, HTTPRoute{Pattern: pattern, Discovery: group.discovery})
		}
	}
	return routes, nil
}
