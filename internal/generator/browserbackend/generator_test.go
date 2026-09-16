package browserbackend

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jsell-rh/stego/internal/browserassets"
	"github.com/jsell-rh/stego/internal/compiler"
	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/healthcheck"
	"github.com/jsell-rh/stego/internal/generator/kubernetesservice"
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
	if err != nil || !reflect.DeepEqual(inputs, []gen.InputFile{{Path: "ui/index.html", MaxBytes: browserassets.MaxFile}, {Path: "ui/main.js", MaxBytes: browserassets.MaxFile}}) {
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
func TestAuthorizationScopes(t *testing.T) {
	g := new(Generator)
	config := fixture().ComponentConfig
	s, err := g.config(config)
	if err != nil || s.OAuthScope != "openid" {
		t.Fatal("default login requests additional privileges", err)
	}
	config["additional_scopes"] = []any{"profile", "email", "records:read"}
	s, err = g.config(config)
	if err != nil || s.OAuthScope != "openid email profile records:read" {
		t.Fatal("declared scopes differ", err)
	}
	for _, value := range []any{nil, "email", []any{""}, []any{"openid"}, []any{"offline_access"}, []any{"email", "email"}, []any{"profile email"}, []any{"email\n"}, []any{true}, []any{strings.Repeat("a", 129)}, []any{"a", "b", "c", "d", "e", "f", "g", "h"}} {
		config["additional_scopes"] = value
		if _, err := g.config(config); err == nil {
			t.Fatalf("invalid scope declaration accepted: %#v", value)
		}
	}
	config["additional_scopes"] = []any{}
	s, err = g.config(config)
	if err != nil || s.OAuthScope != "openid" {
		t.Fatal("empty additional scopes changed OpenID login", err)
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
func TestGeneratedRuntime(t *testing.T)                 { testGeneratedRuntime(t, new(Generator), fixture(), false) }
func TestGeneratedLocalApplicationRuntime(t *testing.T) { testLocalApplicationRuntime(t, false, false) }
func TestGeneratedCapturedApplicationRuntime(t *testing.T) {
	testLocalApplicationRuntime(t, true, false)
}
func TestGeneratedDeclaredApplicationRuntime(t *testing.T) {
	testLocalApplicationRuntime(t, true, true)
}
func TestGeneratedApplicationLoginDocument(t *testing.T) {
	testLocalApplicationRuntime(t, true, false, "^TestApplicationLoginDocumentTargets$")
}
func testLocalApplicationRuntime(t *testing.T, captured, declared bool, pattern ...string) {
	t.Helper()
	var healthStatus atomic.Int32
	healthStatus.Store(200)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/readyz" {
			if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
				w.WriteHeader(400)
				return
			}
			w.WriteHeader(int(healthStatus.Load()))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/socket") {
			if r.Header.Get("Authorization") != "Bearer initial-access-value" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Forwarded-User") != "" {
				w.WriteHeader(401)
				return
			}
			if r.URL.Query().Get("denied") == "1" {
				w.WriteHeader(403)
				return
			}
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer conn.CloseNow()
			for {
				kind, data, err := conn.Read(r.Context())
				if err != nil {
					return
				}
				if string(data) == "exit" {
					_ = conn.Close(websocket.StatusNormalClosure, "17")
					return
				}
				if err := conn.Write(r.Context(), kind, data); err != nil {
					return
				}
			}
		}
		w.Header().Set("Set-Cookie", "upstream=private")
		w.Header().Set("X-Private", "private")
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/unauthorized") {
			w.WriteHeader(401)
			return
		}
		if r.Header.Get("Authorization") != "Bearer initial-access-value" && r.Header.Get("Authorization") != "Bearer refreshed-access-value" {
			w.WriteHeader(401)
			return
		}
		if r.URL.Path == "/api/records/v1/fixture-health-status" {
			switch r.URL.Query().Get("status") {
			case "200":
				healthStatus.Store(200)
			case "503":
				healthStatus.Store(503)
			default:
				w.WriteHeader(400)
				return
			}
			w.WriteHeader(204)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"path": r.URL.RequestURI(), "headers": r.Header, "method": r.Method})
	}))
	defer server.Close()
	_, rawPort, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil {
		t.Fatal(err)
	}
	ctx := fixture()
	if captured {
		ctx.ComponentConfig["telemetry_service_name"] = "captured-application"
	} else {
		delete(ctx.ComponentConfig, "assets")
		ctx.Inputs = nil
	}
	g := &Generator{LocalApplicationPort: port}
	if declared {
		ctx.ServiceName = "browser"
		ctx.PeerNamespaces["browser-backend"] = "browser"
		ctx.PeerNamespaces["kubernetes-service"] = "deploy"
		ctx.PeerConfigs = map[string]map[string]any{
			"browser-backend": ctx.ComponentConfig, "health-check": {"database": true},
			"kubernetes-service": {"local_applications": []any{map[string]any{"port": port, "image": "registry.example.test/app@sha256:" + strings.Repeat("a", 64), "listen_env": "LISTEN_ADDRESS", "port_env": "PORT", "env_secret": "app-env", "files_secret": "app-files", "health_path": "/readyz"}}},
		}
		g = &Generator{}
	}
	testGeneratedRuntime(t, g, ctx, true, pattern...)
}
func TestGeneratedManagedBrowserSchema(t *testing.T) {
	testGeneratedRuntime(t, new(Generator), fixture(), false, "^Test(ManagedBrowserSchema|BrowserSchemaRefusesDrift|BrowserSchemaRefusesRuntimeGrantDrift|BrowserSchemaBootstrapRollsBack)$")
}

func TestGeneratedSchemaInputs(t *testing.T) {
	testGeneratedRuntime(t, new(Generator), fixture(), false, "^TestSchemaInputValidation$")
}

func testGeneratedRuntime(t *testing.T, g *Generator, ctx gen.Context, local bool, pattern ...string) {
	t.Helper()
	project := t.TempDir()
	files, wiring, err := g.Generate(ctx)
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
		ctx := ctx
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
	if ctx.PeerConfigs["kubernetes-service"] != nil {
		deployment := ctx
		deployment.GoVersion = "1.26.8"
		deployment.OutputNamespace = "deploy"
		deployment.ComponentConfig = ctx.PeerConfigs["kubernetes-service"]
		generated, _, err := new(kubernetesservice.Generator).Generate(deployment)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, generated...)
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
		if entry.IsDir() {
			continue
		}
		if local && entry.Name() != "fixture_test.go" {
			continue
		}
		data, err := os.ReadFile(filepath.Join("testdata", entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(project, "out/browser", entry.Name()), data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	if local {
		entries, err := os.ReadDir("testdata/application")
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			data, err := os.ReadFile(filepath.Join("testdata/application", entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(project, "out/browser/application_"+entry.Name()), data, 0644); err != nil {
				t.Fatal(err)
			}
		}
	}
	if ctx.PeerConfigs["kubernetes-service"] != nil {
		data, err := os.ReadFile("testdata/declared/health_test.go")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(project, "out/browser/declared_health_test.go"), data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	checks := [][]string{{"mod", "tidy"}, {"test", "-race", "-count=1", "-mod=readonly", "-timeout=90s", "-v", "./..."}}
	if len(pattern) == 1 {
		checks[1] = append(checks[1], "-run", pattern[0])
	}
	if local && os.Getenv("STEGO_REQUIRE_POSTGRES") == "1" {
		checks = append(checks, []string{"run", "golang.org/x/vuln/cmd/govulncheck@v1.4.0", "./..."})
	}
	for _, args := range checks {
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
	if err != nil || !reflect.DeepEqual(names, []gen.InputFile{{Path: "ui/assets.zip", MaxBytes: browserassets.MaxBundle}}) {
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

func TestBrowserAssemblyUsesDeclaredPool(t *testing.T) {
	ctx := fixture()
	_, backend, err := new(Generator).Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ctx.OutputNamespace = ctx.PeerNamespaces["postgres-adapter"]
	ctx.ComponentConfig = map[string]any{"migrations": "external"}
	files, pool, err := new(postgresadapter.Generator).Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pool == nil || pool.DatabaseOpener == nil {
		t.Fatal("the browser has no declared pool factory without entities")
	}
	shared, err := compiler.Assemble(compiler.AssemblerInput{ModuleName: ctx.ModuleName, OutDirName: ctx.OutDirName, GoVersion: "1.26.8", Wirings: []compiler.ComponentWiring{{Name: "browser-backend", Wiring: backend}, {Name: "postgres-adapter", Wiring: pool}}})
	if err != nil {
		t.Fatal(err)
	}
	var main string
	for _, file := range shared {
		if file.Path == "main.go" {
			main = string(file.Content)
		}
	}
	if !strings.Contains(main, "store.OpenDatabase(dsn)") || strings.Contains(main, `sql.Open("pgx"`) || strings.Contains(main, "gorm.Open(") {
		t.Fatal("the browser bypassed its SQL pool factory")
	}
	if len(files) != 1 || files[0].Path != "store/database.go" {
		t.Fatal("the browser database generated unrelated storage code")
	}
}

func TestLocalApplicationGeneration(t *testing.T) {
	ctx := fixture()
	delete(ctx.ComponentConfig, "assets")
	ctx.Inputs = nil
	g := &Generator{LocalApplicationPort: 8000}
	first, wiring, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	second, again, err := g.Generate(ctx)
	if err != nil || !reflect.DeepEqual(first, second) || !reflect.DeepEqual(wiring, again) {
		t.Fatal("application generation is not stable", err)
	}
	inputs, err := g.InputFiles(ctx.ComponentConfig)
	if err != nil || len(inputs) != 0 {
		t.Fatal("upstream application has embedded inputs", err)
	}
	for _, file := range first {
		if strings.Contains(file.Path, "/public/") {
			t.Fatal("upstream application has embedded assets")
		}
	}
	for _, key := range []string{"assets", "asset_bundle", "telemetry_service_name", "local_application_port"} {
		ctx := ctx
		ctx.ComponentConfig = map[string]any{"api_prefix": "/api/v1", "routes": []any{"/"}, key: "unsupported"}
		if _, _, err := g.Generate(ctx); err == nil {
			t.Fatal("accepted incompatible configuration", key)
		}
	}
	for _, port := range []int{-1, 80, 65536} {
		if _, _, err := (&Generator{LocalApplicationPort: port}).Generate(ctx); err == nil {
			t.Fatal("accepted invalid application port", port)
		}
	}
}

func TestCapturedApplicationGeneration(t *testing.T) {
	ctx := fixture()
	ctx.ComponentConfig["telemetry_service_name"] = "captured-application"
	g := &Generator{LocalApplicationPort: 8000}
	first, wiring, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	second, again, err := g.Generate(ctx)
	if err != nil || !reflect.DeepEqual(first, second) || !reflect.DeepEqual(wiring, again) {
		t.Fatal("captured application generation changed", err)
	}
	config, _, err := g.resolveAssets(ctx)
	if err != nil || len(config.ScriptHashes) != 1 || config.RuntimeConfigOffset == 0 {
		t.Fatal("captured application lost its content checks", err)
	}
	ctx.Inputs["ui/index.html"] = []byte(`<html><head></head><script src="https://external.example/a.js"></script></html>`)
	if _, _, err := g.Generate(ctx); err == nil {
		t.Fatal("captured application accepted an external script")
	}
	bundle, err := browserassets.Encode([]browserassets.Asset{{Path: "index.html", Data: fixture().Inputs["ui/index.html"]}, {Path: "assets/main.js", Data: []byte(`"use strict";`)}})
	if err != nil {
		t.Fatal(err)
	}
	delete(ctx.ComponentConfig, "assets")
	ctx.ComponentConfig["asset_bundle"] = "ui/app.zip"
	ctx.Inputs = map[string][]byte{"ui/app.zip": bundle}
	if _, _, err := g.Generate(ctx); err != nil {
		t.Fatal(err)
	}
}
