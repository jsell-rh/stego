package healthcheck_test

import (
	_ "embed"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/jsell-rh/stego/internal/compiler"
	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/healthcheck"
)

//go:embed testdata/health_test.go
var runtimeTests []byte

//go:embed testdata/service_test.go
var serviceTests []byte

func TestGeneratedHealth(t *testing.T) {
	ctx := gen.Context{ModuleName: "example.com/probes", OutputNamespace: "health"}
	files, wiring, err := new(healthcheck.Generator).Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	authIndex := 0
	assembled, err := compiler.Assemble(compiler.AssemblerInput{ModuleName: ctx.ModuleName, GoVersion: "1.25.0", Wirings: []compiler.ComponentWiring{{Name: "health-check", Wiring: wiring}, {Name: "guard", Wiring: &gen.Wiring{
		Imports: []string{"guard"}, Constructors: []string{"guard.NewAuthMiddleware()", "guard.NewHandler()"}, MiddlewareConstructor: &authIndex, MiddlewareWrapExpr: "%s(%s)", Routes: []string{`mux.Handle("GET /private", handler)`},
	}}}})
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, assembled...)
	files = append(files, gen.File{Path: "health/health_test.go", Content: runtimeTests}, gen.File{Path: "service_test.go", Content: serviceTests}, gen.File{Path: "guard/guard.go", Content: []byte(`package guard
import "net/http"
func NewAuthMiddleware()func(http.Handler)http.Handler{return func(next http.Handler)http.Handler{return http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){if r.Header.Get("X-Test-Authorized")=="yes"{next.ServeHTTP(w,r);return};w.WriteHeader(401)})}}
func NewHandler()http.Handler{return http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){w.WriteHeader(204)})}
`)})
	project := t.TempDir()
	for _, file := range files {
		p := filepath.Join(project, file.Path)
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, file.Bytes(), 0644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"vet", "./..."}, {"test", "-race", "-count=1", "-timeout=30s", "./..."}} {
		cmd := exec.Command("go", args...)
		cmd.Dir = project
		cmd.Env = append(os.Environ(), "GOWORK=off")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("generated health: %v\n%s", err, output)
		}
	}
}
func TestInvalidHealthInputs(t *testing.T) {
	for _, ctx := range []gen.Context{{}, {ModuleName: "example.com/probes", OutputNamespace: "../health"}, {ModuleName: "example.com/probes", OutputNamespace: "health", ComponentConfig: map[string]any{"database": "true"}}, {ModuleName: "example.com/probes", OutputNamespace: "health", ComponentConfig: map[string]any{"databse": true}}} {
		g := new(healthcheck.Generator)
		if err := g.ValidateContext(ctx); err == nil {
			t.Fatal("invalid input accepted")
		}
		if files, _, err := g.Generate(ctx); err == nil || len(files) != 0 {
			t.Fatal("invalid input rendered output")
		}
	}
}
