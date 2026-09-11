package browserbackend

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jsell-rh/stego/internal/browserassets"
	"github.com/jsell-rh/stego/internal/compiler"
	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/healthcheck"
	"github.com/jsell-rh/stego/internal/generator/oteltracing"
	"github.com/jsell-rh/stego/internal/generator/postgresadapter"
)

func fixture() gen.Context {
	return gen.Context{ModuleName: "example.com/browser-test", OutDirName: "out", OutputNamespace: "browser", PeerNamespaces: map[string]string{"postgres-adapter": "store", "otel-tracing": "tracing", "health-check": "health"}, ComponentConfig: map[string]any{"api_prefix": "/api/records/v1", "routes": []any{"/", "/records/{id}"}, "assets": []any{map[string]any{"source": "ui/index.html", "path": "/index.html"}, map[string]any{"source": "ui/main.js", "path": "/assets/main.js"}}}, Inputs: map[string][]byte{"ui/index.html": []byte(`<!doctype html><html><head></head><body><script src="/assets/main.js"></script><script>window.ready=true;</script></body></html>`), "ui/main.js": []byte(`"use strict";`)}}
}

func TestGeneration(t *testing.T) {
	g := new(Generator)
	ctx := fixture()
	files, wiring, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	again, other, err := g.Generate(ctx)
	if err != nil || !reflect.DeepEqual(files, again) || !reflect.DeepEqual(wiring, other) {
		t.Fatal("generation is not stable", err)
	}
	inputs, err := g.InputFiles(ctx.ComponentConfig)
	if err != nil || !reflect.DeepEqual(inputs, []string{"ui/index.html", "ui/main.js"}) {
		t.Fatal(inputs, err)
	}
	if !wiring.NeedsDB || !wiring.ConstructorReturnsError[0] || !reflect.DeepEqual(wiring.ConstructorResources[0], []gen.Resource{gen.ServiceContext, gen.SQLDatabase}) || wiring.ConstructorDeferCalls[0] != "Close()" || !reflect.DeepEqual(wiring.BackgroundTasks, []int{0}) {
		t.Fatal("missing runtime resource contract")
	}
	if g.MinimumGoVersion() != "1.26.8" {
		t.Fatal("unexpected toolchain")
	}
	for _, file := range files {
		if strings.HasSuffix(file.Path, "oauth.go") && !bytes.Contains(file.Bytes(), []byte("func validUnicodeEscapes")) {
			t.Fatal("missing strict JSON boundary")
		}
	}
}
func TestInvalidConfig(t *testing.T) {
	cases := map[string]func(*gen.Context){
		"telemetry service":  func(c *gen.Context) { c.ComponentConfig["telemetry_service_name"] = "invalid service" },
		"reserved telemetry": func(c *gen.Context) { c.ComponentConfig["routes"] = []any{"/", "/telemetry/v1/traces"} },
		"logout scope":       func(c *gen.Context) { c.ComponentConfig["logout_scope"] = "all" },
		"unknown":            func(c *gen.Context) { c.ComponentConfig["tokens_in_cookie"] = true },
		"no database":        func(c *gen.Context) { delete(c.PeerNamespaces, "postgres-adapter") },
		"no telemetry":       func(c *gen.Context) { delete(c.PeerNamespaces, "otel-tracing") },
		"mixed API":          func(c *gen.Context) { c.PeerNamespaces["jwt-auth"] = "auth" },
		"asset missing":      func(c *gen.Context) { delete(c.Inputs, "ui/main.js") },
		"asset too large":    func(c *gen.Context) { c.Inputs["ui/main.js"] = make([]byte, (4<<20)+1) },
		"generated input": func(c *gen.Context) {
			c.ComponentConfig["assets"].([]any)[0].(map[string]any)["source"] = "out/input.html"
		},
		"traversal": func(c *gen.Context) {
			c.ComponentConfig["assets"].([]any)[0].(map[string]any)["source"] = "../input.html"
		},
		"asset outside public": func(c *gen.Context) { c.ComponentConfig["assets"].([]any)[1].(map[string]any)["path"] = "/script.js" },
		"unsupported asset": func(c *gen.Context) {
			c.ComponentConfig["assets"].([]any)[1].(map[string]any)["path"] = "/assets/secret.pem"
		},
		"missing root":      func(c *gen.Context) { c.ComponentConfig["routes"] = []any{"/records"} },
		"unknown parameter": func(c *gen.Context) { c.ComponentConfig["routes"] = []any{"/", "/records/{other}"} },
		"duplicate route":   func(c *gen.Context) { c.ComponentConfig["routes"] = []any{"/", "/"} },
		"roles type":        func(c *gen.Context) { c.ComponentConfig["roles_claim"] = false },
	}
	for _, prefix := range []string{"/", "/auth", "/auth/session", "/assets", "/index.html", "/readyz", "/livez", "/api/../auth", "/api/{id}"} {
		cases["prefix "+prefix] = func(c *gen.Context) { c.ComponentConfig["api_prefix"] = prefix }
	}
	for _, route := range []string{"/auth/callback", "/api/records/v1/a", "/assets/x", "/livez", "/readyz", "/index.html"} {
		cases["route "+route] = func(c *gen.Context) { c.ComponentConfig["routes"] = []any{"/", route} }
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := fixture()
			change(&ctx)
			if _, _, err := new(Generator).Generate(ctx); err == nil {
				t.Fatal("invalid input was accepted")
			}
		})
	}
}
func TestGeneratedRuntime(t *testing.T) {
	project := t.TempDir()
	files, wiring, err := new(Generator).Generate(fixture())
	if err != nil {
		t.Fatal(err)
	}
	wirings := []compiler.ComponentWiring{{Name: "browser-backend", Wiring: wiring}}
	for _, peer := range []struct {
		name      string
		generator gen.Generator
		config    map[string]any
	}{
		{"postgres-adapter", new(postgresadapter.Generator), map[string]any{"migrations": "external"}},
		{"otel-tracing", new(oteltracing.Generator), nil},
		{"health-check", new(healthcheck.Generator), map[string]any{"database": true}},
	} {
		ctx := fixture()
		ctx.ServiceName = "browser"
		ctx.OutputNamespace = ctx.PeerNamespaces[peer.name]
		ctx.ComponentConfig = peer.config
		generated, peerWiring, err := peer.generator.Generate(ctx)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, generated...)
		wirings = append(wirings, compiler.ComponentWiring{Name: peer.name, Wiring: peerWiring})
	}
	shared, err := compiler.Assemble(compiler.AssemblerInput{ModuleName: fixture().ModuleName, OutDirName: "out", GoVersion: "1.26.8", Wirings: wirings})
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, shared...)
	for _, file := range files {
		name := filepath.Join(project, "out", file.Path)
		if file.Path == "go.mod" {
			name = filepath.Join(project, file.Path)
		}
		if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, file.Bytes(), 0644); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join("testdata", entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(project, "out/browser", entry.Name()), data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"mod", "tidy"}, {"test", "-race", "-count=1", "-mod=readonly", "-timeout=90s", "-v", "./..."}} {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		cmd := exec.CommandContext(ctx, "go", args...)
		cmd.Dir = project
		cmd.Env = append(os.Environ(), "GOWORK=off")
		out, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("generated runtime %v: %v\n%s", args, err, out)
		}
		t.Logf("%s", out)
	}
}

func TestAssetBundle(t *testing.T) {
	ctx := fixture()
	g := new(Generator)
	bundle, err := browserassets.Encode([]browserassets.Asset{{Path: "index.html", Data: []byte(`<html><script>window.ready=true;</script></html>`)}})
	if err != nil {
		t.Fatal(err)
	}
	delete(ctx.ComponentConfig, "assets")
	ctx.ComponentConfig["asset_bundle"] = "ui/assets.zip"
	ctx.Inputs = map[string][]byte{"ui/assets.zip": bundle}
	names, err := g.InputFiles(ctx.ComponentConfig)
	if err != nil || !reflect.DeepEqual(names, []string{"ui/assets.zip"}) {
		t.Fatal("bundle is not captured", names, err)
	}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, file := range files {
		if file.Path == "browser/backend.go" {
			found = bytes.Contains(file.Bytes(), []byte("sha256-"))
		}
	}
	if !found {
		t.Fatal("captured inline script has no CSP hash")
	}
	ctx.ComponentConfig["assets"] = []any{}
	if g.ValidateContext(ctx) == nil {
		t.Fatal("ambiguous asset source accepted")
	}
}

func TestRuntimeConfigBoundary(t *testing.T) {
	for _, html := range []string{`<html><body>none</body></html>`, `<html><head></head><head></head></html>`, `<html><head><meta name="stego-runtime-config" content="private"></head></html>`} {
		ctx := fixture()
		ctx.ComponentConfig["telemetry_service_name"] = "example-browser"
		ctx.Inputs["ui/index.html"] = []byte(html)
		if new(Generator).ValidateContext(ctx) == nil {
			t.Fatal("ambiguous runtime configuration accepted")
		}
	}
	ctx := fixture()
	ctx.ComponentConfig["telemetry_service_name"] = "example-browser"
	if err := new(Generator).ValidateContext(ctx); err != nil {
		t.Fatal(err)
	}
}
