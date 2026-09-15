package restapi

import (
	"go/ast"
	"go/parser"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
)

func basePathMux(wiring *gen.Wiring) (mux *http.ServeMux, failure any) {
	defer func() { failure = recover() }()
	mux = http.NewServeMux()
	for _, statement := range append(append([]string{}, wiring.Routes...), wiring.DiscoveryRoutes...) {
		expression, err := parser.ParseExpr(statement)
		if err != nil {
			panic(err)
		}
		literal := expression.(*ast.CallExpr).Args[0].(*ast.BasicLit)
		pattern, err := strconv.Unquote(literal.Value)
		if err != nil {
			panic(err)
		}
		mux.HandleFunc(pattern, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	}
	return mux, nil
}

func TestRejectAmbiguousBasePathsBeforeRendering(t *testing.T) {
	for _, prefix := range []string{
		"/api/{id}", "/api/{tail...}", "/api/{", "/api//v1", "/api/./v1", "/api/../v1",
		"/api/v1/", "//api", "/", "/api?mode=x", "/api#fragment", "/api/%2f", "/api/%7Bid%7D",
		"/api/%", "/api\\v1", "/api\nv1", "/api\tv1", "/api\"v1", "/api v1", "/café",
	} {
		t.Run(strconv.Quote(prefix), func(t *testing.T) {
			ctx := basicContext()
			ctx.BasePath = prefix
			files, wiring, err := (&Generator{}).Generate(ctx)
			if err == nil {
				_, failure := basePathMux(wiring)
				t.Fatalf("accepted ambiguous base_path; generated router result: %v", failure)
			}
			if !strings.Contains(err.Error(), "base_path") || len(files) != 0 || wiring != nil {
				t.Fatalf("invalid prefix produced output or an unrelated error: %v", err)
			}
			ctx.Collections = nil
			if err := (&Generator{}).ValidateContext(ctx); err == nil {
				t.Fatal("invalid prefix was accepted without collections")
			}
		})
	}
}

func TestLiteralBasePathsRegisterAndServeExactRoutes(t *testing.T) {
	for _, prefix := range []string{"", "/api/v1", "/api/user-mgmt/v1", "/API/v1.0", "/api/_shared/~preview", "/.well-known/service"} {
		t.Run(prefix, func(t *testing.T) {
			ctx := basicContext()
			ctx.BasePath = prefix
			_, wiring, err := (&Generator{}).Generate(ctx)
			if err != nil {
				t.Fatal(err)
			}
			mux, failure := basePathMux(wiring)
			if failure != nil {
				t.Fatal("generated route registration failed", failure)
			}
			for _, path := range []string{prefix + "/users/123", prefix + "/openapi"} {
				response := httptest.NewRecorder()
				mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
				if response.Code != http.StatusNoContent {
					t.Fatal("generated route did not match its literal path", path, response.Code)
				}
			}
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/unrelated/users/123", nil))
			if response.Code != http.StatusNotFound {
				t.Fatal("generated route matched an unrelated prefix", response.Code)
			}
		})
	}
}
