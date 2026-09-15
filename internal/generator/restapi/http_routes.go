package restapi

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/types"
)

type httpRoute struct {
	method, path, handler string
}

func (*Generator) HTTPRoutes(ctx gen.Context) ([]gen.HTTPRoute, error) {
	if len(ctx.Collections) == 0 {
		return nil, nil
	}
	collections := map[string]types.Collection{}
	for _, collection := range ctx.Collections {
		if _, exists := collections[collection.Entity]; !exists {
			collections[collection.Entity] = collection
		}
	}
	var routes []gen.HTTPRoute
	for _, collection := range ctx.Collections {
		base, err := collectionBasePath(collection, collections)
		if err != nil {
			return nil, err
		}
		for _, operation := range collection.Operations {
			route, err := collectionHTTPRoute(operation, ctx.BasePath+base)
			if err != nil {
				return nil, err
			}
			routes = append(routes, gen.HTTPRoute{Pattern: route.pattern()})
		}
	}
	for _, route := range discoveryHTTPRoutes(ctx.BasePath) {
		routes = append(routes, gen.HTTPRoute{Pattern: route.pattern(), Discovery: true})
	}
	return routes, nil
}

func (r httpRoute) pattern() string { return r.method + " " + r.path }

func collectionHTTPRoute(operation types.Operation, base string) (httpRoute, error) {
	switch operation {
	case types.OpCreate:
		return httpRoute{"POST", base, "Create"}, nil
	case types.OpRead:
		return httpRoute{"GET", base + "/{id}", "Read"}, nil
	case types.OpUpdate:
		return httpRoute{"PUT", base + "/{id}", "Update"}, nil
	case types.OpDelete:
		return httpRoute{"DELETE", base + "/{id}", "Delete"}, nil
	case types.OpList:
		return httpRoute{"GET", base, "List"}, nil
	case types.OpUpsert:
		return httpRoute{"PUT", base, "Upsert"}, nil
	case types.OpPatch:
		return httpRoute{"PATCH", base + "/{id}", "Patch"}, nil
	default:
		return httpRoute{}, fmt.Errorf("unsupported collection route operation %q", operation)
	}
}

func discoveryHTTPRoutes(base string) []httpRoute {
	metadata := base
	if metadata == "" {
		metadata = "/{$}"
	}
	return []httpRoute{
		{"GET", base + "/openapi", "ServeOpenAPI"},
		{"GET", base + "/openapi.html", "ServeOpenAPIUI"},
		{"GET", metadata, "ServeMetadata"},
	}
}

func validateCollectionHTTPPath(value string) error {
	if !strings.HasPrefix(value, "/") {
		return fmt.Errorf("collection path must start with '/'")
	}
	names := map[string]bool{}
	for segment := range strings.SplitSeq(value[1:], "/") {
		if strings.HasPrefix(segment, "{") && strings.HasSuffix(segment, "}") {
			name := segment[1 : len(segment)-1]
			if name == "" || name == "id" || names[name] {
				return fmt.Errorf("collection path parameters must be unique and cannot use the reserved name id")
			}
			for i := 0; i < len(name); i++ {
				c := name[i]
				if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' || (i > 0 && c >= '0' && c <= '9')) {
					return fmt.Errorf("collection path parameters must be ASCII identifiers")
				}
			}
			names[name] = true
			continue
		}
		if err := gen.ValidateHTTPBasePath("/" + segment); err != nil {
			return fmt.Errorf("collection path must contain canonical literal segments or single-segment parameters")
		}
	}
	return nil
}

func registerHTTPRoute(mux *http.ServeMux, route httpRoute) (err error) {
	defer func() {
		if failure := recover(); failure != nil {
			err = fmt.Errorf("invalid or conflicting route %q: %v", route.pattern(), failure)
		}
	}()
	mux.HandleFunc(route.pattern(), func(http.ResponseWriter, *http.Request) {})
	return nil
}

func validateHTTPRoutePatterns(ctx gen.Context, collections map[string]types.Collection) error {
	// A legacy GODEBUG setting must not silently disable pattern validation.
	if registerHTTPRoute(http.NewServeMux(), httpRoute{method: "GET", path: "/{"}) == nil {
		return fmt.Errorf("REST route validation requires Go 1.22 HTTP pattern semantics")
	}
	mux := http.NewServeMux()
	for _, collection := range ctx.Collections {
		path, err := collectionBasePath(collection, collections)
		if err != nil {
			return err
		}
		if err := validateCollectionHTTPPath(path); err != nil {
			return fmt.Errorf("collection %q path_prefix: %w", collection.Name, err)
		}
		for _, operation := range collection.Operations {
			route, err := collectionHTTPRoute(operation, ctx.BasePath+path)
			if err != nil {
				return err
			}
			if err := registerHTTPRoute(mux, route); err != nil {
				return fmt.Errorf("collection %q: %w", collection.Name, err)
			}
		}
	}
	for _, route := range discoveryHTTPRoutes(ctx.BasePath) {
		path := route.path
		if path == "/{$}" {
			path = "/"
		}
		request, err := http.NewRequest(route.method, "https://stego.invalid"+path, nil)
		if err != nil {
			return fmt.Errorf("invalid discovery route")
		}
		if _, matched := mux.Handler(request); matched != "" {
			return fmt.Errorf("collection route %q is hidden by public discovery route %q", matched, route.pattern())
		}
		if err := registerHTTPRoute(mux, route); err != nil {
			return err
		}
	}
	return nil
}
