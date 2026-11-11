package keycloakprovider

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/controller"
	"github.com/jsell-rh/stego/internal/generator/httpclient"
	"github.com/jsell-rh/stego/internal/generator/jwtauth"
	"github.com/jsell-rh/stego/internal/generator/oteltracing"
)

func fixture() gen.Context {
	return gen.Context{ModuleName: "example.com/provider", OutDirName: "out", OutputNamespace: "keycloak", AuthPackage: "example.com/provider/out/auth", PeerNamespaces: map[string]string{"http-application": "application", "jwt-auth": "auth"}}
}

func managedFixture() gen.Context {
	c := fixture()
	c.ComponentConfig = map[string]any{"managed_clients": []any{
		map[string]any{"kind": "service-account", "client_id": "hs-sa-{gateway}-{account}", "ids": []any{"gateway", "account"},
			"attributes": []any{
				map[string]any{"key": "hypershell.service-account", "value": "true"},
				map[string]any{"key": "hypershell.gateway-id", "value": "{gateway}"},
				map[string]any{"key": "hypershell.service-account-id", "value": "{account}"},
			}, "legacy": true, "legacy_client_id": true},
		map[string]any{"kind": "gateway", "client_id": "hs-gateway-{gateway}", "ids": []any{"gateway"},
			"attributes": []any{
				map[string]any{"key": "hypershell.gateway", "value": "true"},
				map[string]any{"key": "hypershell.gateway-id", "value": "{gateway}"},
			}, "legacy": true, "legacy_client_id": true},
		map[string]any{"kind": "console", "client_id": "hs-console-{gateway}", "ids": []any{"gateway"},
			"attributes": []any{
				map[string]any{"key": "hypershell.console", "value": "true"},
				map[string]any{"key": "hypershell.gateway-id", "value": "{gateway}"},
			}},
	}}
	return c
}

func TestProviderValidation(t *testing.T) {
	for _, change := range []func(*gen.Context){
		func(c *gen.Context) { c.AuthPackage = "" },
		func(c *gen.Context) { c.AuthPackage = "example.com/foreign/auth" },
		func(c *gen.Context) { c.OutputNamespace = "bad-name" },
		func(c *gen.Context) { c.PeerNamespaces = nil },
		func(c *gen.Context) { c.PeerNamespaces["http-application"] = "bad-name" },
		func(c *gen.Context) { c.ModuleName = "" },
		func(c *gen.Context) { c.PeerNamespaces["controller"] = "../outside" },
		func(c *gen.Context) { c.ComponentConfig = map[string]any{"gateway_role": "admin"} },
		func(c *gen.Context) {
			c.ComponentConfig = map[string]any{"managed_clients": []any{map[string]any{"kind": "gateway", "client_id": "{gateway}", "ids": []any{"gateway"}, "attributes": []any{map[string]any{"key": "hypershell.gateway", "value": "{missing}"}}}}}
		},
		func(c *gen.Context) {
			c.ComponentConfig = map[string]any{"managed_clients": []any{map[string]any{"kind": "Gateway", "client_id": "gw-{gateway}", "ids": []any{"gateway"}, "attributes": []any{map[string]any{"key": "k", "value": "v"}}}}}
		},
		func(c *gen.Context) { c.ComponentConfig = map[string]any{"managed_clients": "gateway"} },
	} {
		c := fixture()
		change(&c)
		g := new(Generator)
		checked := g.ValidateContext(c)
		if checked == nil {
			t.Fatal("invalid provider declaration accepted")
		}
		files, wiring, err := g.Generate(c)
		if err == nil || err.Error() != checked.Error() || files != nil || wiring != nil {
			t.Fatal("generation bypassed preflight")
		}
	}
}

func TestGeneratedBrowserClient(t *testing.T) {
	testGeneratedProvider(t, false, false, "^TestBrowser")
}

func TestGeneratedAccessLifecycle(t *testing.T) {
	for _, telemetry := range []bool{false, true} {
		t.Run(fmt.Sprint(telemetry), func(t *testing.T) {
			testGeneratedProvider(t, telemetry, false, "^Test(AccessAbnormalExit|NativeAccess|ServiceAccountAccess)")
		})
	}
}

func TestGeneratedServiceAccountLifecycle(t *testing.T) {
	for _, telemetry := range []bool{false, true} {
		t.Run(fmt.Sprint(telemetry), func(t *testing.T) { testGeneratedProvider(t, telemetry, false, "^TestServiceAccountLifecycle") })
	}
}

func TestGeneratedNativeLifecycle(t *testing.T) {
	for _, telemetry := range []bool{false, true} {
		t.Run(fmt.Sprint(telemetry), func(t *testing.T) { testGeneratedProvider(t, telemetry, false, "^TestNativeLifecycle") })
	}
}
func TestGeneratedClientCursor(t *testing.T) {
	testGeneratedProvider(t, false, false, "^TestClientCursor")
}
func TestGeneratedClientNameSearch(t *testing.T) {
	testGeneratedProvider(t, false, false, "^Test(BoundedClientNameSearch|ClientNameSearchDoesNotExpandFailedQueries)$")
}
func TestGeneratedManagedClients(t *testing.T) {
	testGeneratedProviderWith(t, managedFixture, false, false, "^TestManaged")
}

func TestGeneratedKeycloakProviderLive(t *testing.T) {
	if os.Getenv("STEGO_REQUIRE_KEYCLOAK_PROVIDER") != "1" {
		t.Skip("real Keycloak provider gate requires CI")
	}
	if os.Getenv("GITHUB_ACTIONS") != "true" {
		t.Fatal("the real Keycloak provider gate runs only in CI")
	}
	testGeneratedProvider(t, true, true)
}
func testGeneratedProvider(t *testing.T, telemetry, live bool, patterns ...string) {
	testGeneratedProviderWith(t, fixture, telemetry, live, patterns...)
}
func testGeneratedProviderWith(t *testing.T, supply func() gen.Context, telemetry, live bool, patterns ...string) {
	g := new(Generator)
	c := supply()
	withLifecycle := telemetry || live || len(patterns) != 0
	if withLifecycle {
		c.PeerNamespaces["controller"] = "controller"
	}
	files, wiring, err := g.Generate(c)
	if err != nil {
		t.Fatal(err)
	}
	again, other, err := g.Generate(c)
	if err != nil || !reflect.DeepEqual(files, again) || !reflect.DeepEqual(wiring, other) {
		t.Fatal("unstable provider generation", err)
	}
	if withLifecycle {
		peer := c
		peer.OutputNamespace = "controller"
		peer.ComponentConfig = nil
		generated, _, err := new(controller.Generator).Generate(peer)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, generated...)
	}
	tracingImport := ""
	var module strings.Builder
	module.WriteString("module example.com/provider\ngo 1.26.11\n")
	authContext := c
	authContext.OutputNamespace = "auth"
	authContext.ComponentConfig = map[string]any{"mode": "verifier"}
	authFiles, authWiring, err := new(jwtauth.Generator).Generate(authContext)
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, authFiles...)
	requires := authWiring.GoModRequires
	if telemetry {
		tracingImport = "example.com/provider/out/tracing"
		peer := c
		peer.OutputNamespace = "tracing"
		peer.ServiceName = "provider-test"
		generated, wiring, err := new(oteltracing.Generator).Generate(peer)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, generated...)
		// The live native login fixture parses Keycloak HTML forms.
		wiring.GoModRequires["golang.org/x/net"] = "v0.60.0"
		for key, value := range wiring.GoModRequires {
			requires[key] = value
		}
		files = append(files, gen.File{Path: "tracing/test_fixture.go", Content: []byte(`package tracing
import (sdktrace "go.opentelemetry.io/otel/sdk/trace"; "go.opentelemetry.io/otel/sdk/trace/tracetest")
func ProviderTestRuntime()(*Runtime,*tracetest.SpanRecorder){
 recorder:=tracetest.NewSpanRecorder()
 r:=&Runtime{provider:sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))}
 if err:=r.initHTTPClientSignals();err!=nil{panic(err)}
 return r,recorder
}
`)})
	}
	keys := make([]string, 0, len(requires))
	for key := range requires {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	module.WriteString("require (\n")
	for _, key := range keys {
		fmt.Fprintf(&module, "%s %s\n", key, requires[key])
	}
	module.WriteString(")\n")
	h, err := httpclient.Render("application/client", tracingImport, httpclient.FollowLinks)
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, h)
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !withLifecycle && (strings.HasPrefix(entry.Name(), "client_cursor") || strings.HasPrefix(entry.Name(), "native_lifecycle") || strings.HasPrefix(entry.Name(), "browser_lifecycle") || strings.HasPrefix(entry.Name(), "service_account_lifecycle")) {
			continue
		}
		if c.ComponentConfig == nil && strings.HasPrefix(entry.Name(), "managed_clients") {
			continue
		}
		if !telemetry && (entry.Name() == "trace_test.go" || entry.Name() == "live_test.go" || strings.HasSuffix(entry.Name(), "_live_test.go")) {
			continue
		}
		data, err := os.ReadFile(filepath.Join("testdata", entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, gen.File{Path: filepath.Join("keycloak", entry.Name()), Content: data})
	}
	project := t.TempDir()
	for _, file := range files {
		target := filepath.Join(project, "out", file.Path)
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, file.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(project, "go.mod"), []byte(module.String()), 0600); err != nil {
		t.Fatal(err)
	}
	{
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		cmd := exec.CommandContext(ctx, "go", "mod", "tidy")
		cmd.Dir = project
		cmd.Env = append(os.Environ(), "GOWORK=off")
		output, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("provider dependencies: %v\n%s", err, output)
		}
	}
	budget := 60 * time.Second
	args := []string{"test", "-p=1", "-race", "-count=1", "-timeout=30s", "-v"}
	if live {
		budget = 7 * time.Minute
		args = []string{"test", "-p=1", "-race", "-count=1", "-timeout=6m", "-run", "^TestProviderLive$", "-v"}
	}
	if len(patterns) != 0 {
		args = append(args, "-run", strings.Join(patterns, "|"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", append(args, "./...")...)
	cmd.Dir = project
	cmd.Env = append(os.Environ(), "GOWORK=off")
	output, err := cmd.CombinedOutput()
	if live {
		if destination := os.Getenv("STEGO_KEYCLOAK_PROVIDER_ARTIFACTS"); destination != "" {
			if !filepath.IsAbs(destination) {
				t.Fatal("provider artifacts require an absolute path")
			}
			if e := os.MkdirAll(destination, 0700); e != nil {
				t.Fatal(e)
			}
			if e := os.WriteFile(filepath.Join(destination, "runtime.log"), output, 0600); e != nil {
				t.Fatal(e)
			}
			if e := os.CopyFS(filepath.Join(destination, "generated"), os.DirFS(project)); e != nil {
				t.Fatal(e)
			}
		}
	}
	if err != nil {
		t.Fatalf("generated Keycloak provider: %v\n%s", err, output)
	}
	if live && (!bytes.Contains(output, []byte("--- PASS: TestProviderLive ")) || bytes.Contains(output, []byte("--- SKIP: TestProviderLive"))) {
		t.Fatal("real Keycloak provider result is missing")
	}
	t.Logf("%s", output)
}

// managedWith gives the context of one managed gateway kind with the client_id and the attribute key given.
func managedWith(clientID, key string) gen.Context {
	c := fixture()
	c.ComponentConfig = map[string]any{"managed_clients": []any{map[string]any{"kind": "gateway", "client_id": clientID, "ids": []any{"gateway"},
		"attributes": []any{map[string]any{"key": key, "value": "{gateway}"}}}}}
	return c
}

func TestManagedClientForms(t *testing.T) {
	t.Run("ST-SUP-RC13-OC18-T02 A managed-client attribute key or client_id placeholder outside its form is refused before generation", func(t *testing.T) {
		cases := map[string]gen.Context{
			"a key that closes the Go string":           managedWith("gw-{gateway}", `x": "y"}; panic(1); _ = map[string]string{"z`),
			"a key with an upper-case letter":           managedWith("gw-{gateway}", "Gateway"),
			"a key with a newline":                      managedWith("gw-{gateway}", "gateway\nid"),
			"a key of 112 characters":                   managedWith("gw-{gateway}", strings.Repeat("a", 112)),
			"a key that starts with a dot":              managedWith("gw-{gateway}", ".gateway"),
			"a client_id placeholder with code":         managedWith("gw-{func()string{panic(1)}()}", "hypershell.gateway"),
			"a client_id placeholder not an id":         managedWith("gw-{account}", "hypershell.gateway"),
			"a client_id with a lone brace":             managedWith("gw-{gateway}}", "hypershell.gateway"),
			"a client_id placeholder that calls panic":  managedWith("gw-{panic(1)}", "hypershell.gateway"),
			"a client_id placeholder that converts":     managedWith("gw-{string(rune(65))}", "hypershell.gateway"),
			"a client_id placeholder with spaces":       managedWith("gw-{ gateway }", "hypershell.gateway"),
			"an empty client_id placeholder":            managedWith("gw-{}", "hypershell.gateway"),
			"a client_id placeholder that slices an id": managedWith("gw-{gateway[:0]}", "hypershell.gateway"),
		}
		for name, c := range cases {
			g := new(Generator)
			checked := g.ValidateContext(c)
			files, _, err := g.Generate(c)
			if err == nil || files != nil {
				t.Errorf("%s: generation accepted it", name)
			}
			if checked == nil {
				t.Errorf("%s: the context check accepted it", name)
			}
		}
		files, _, err := new(Generator).Generate(managedWith("gw-{gateway}-x", "a.b-c_d"+strings.Repeat("e", 104)))
		if err != nil {
			t.Fatalf("a key of 111 characters in the form was refused: %v", err)
		}
		for _, f := range files {
			if strings.HasSuffix(f.Path, "managed_clients.go") && !strings.Contains(string(f.Content), `ClientID: "gw-" + gateway + "-x",`) {
				t.Errorf("the client name does not keep its literal after the placeholder:\n%s", f.Content)
			}
		}
	})
	t.Run("ST-SUP-RC13-OC18-T03 The emitted identity helpers write each attribute key as a quoted Go string", func(t *testing.T) {
		key := `x": "y"}; panic(1); _ = map[string]string{"z`
		kind := managedKind{Kind: "gateway", Name: "Gateway", IDs: []string{"gateway"}, ClientID: "gw-{gateway}", Legacy: true,
			Attributes: []managedAttribute{{Key: key, Value: "{gateway}", ValueExpr: "gateway"}}}
		data := struct {
			Package     string
			Managed     []managedKind
			LegacyKinds bool
		}{"keycloak", []managedKind{kind}, false}
		code, err := render("managed_clients.go", data)
		if err != nil {
			t.Fatalf("the template did not render a key with quotes: %v", err)
		}
		for _, want := range []string{strconv.Quote("stego.owner."+key) + ": gateway", strconv.Quote(key) + ": gateway",
			strconv.Quote(key) + ": " + strconv.Quote("stego.owner."+key)} {
			if !strings.Contains(string(code), want) {
				t.Errorf("the emitted helpers do not hold %s:\n%s", want, code)
			}
		}
		file, err := parser.ParseFile(token.NewFileSet(), "managed_clients.go", code, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			if call, ok := node.(*ast.CallExpr); ok {
				if name, ok := call.Fun.(*ast.Ident); ok && name.Name == "panic" {
					t.Errorf("the key reached the code as a call of panic:\n%s", code)
				}
			}
			return true
		})
	})
}
