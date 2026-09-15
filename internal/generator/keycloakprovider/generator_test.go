package keycloakprovider

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/httpclient"
	"github.com/jsell-rh/stego/internal/generator/oteltracing"
)

func fixture() gen.Context {
	return gen.Context{ModuleName: "example.com/provider", OutDirName: "out", OutputNamespace: "keycloak", PeerNamespaces: map[string]string{"http-application": "application"}}
}

func TestProviderValidation(t *testing.T) {
	for _, change := range []func(*gen.Context){
		func(c *gen.Context) { c.OutputNamespace = "bad-name" },
		func(c *gen.Context) { c.PeerNamespaces = nil },
		func(c *gen.Context) { c.PeerNamespaces["http-application"] = "bad-name" },
		func(c *gen.Context) { c.ModuleName = "" },
		func(c *gen.Context) { c.ComponentConfig = map[string]any{"gateway_role": "admin"} },
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

func TestGeneratedKeycloakProvider(t *testing.T)          { testGeneratedProvider(t, false, false) }
func TestGeneratedKeycloakProviderTelemetry(t *testing.T) { testGeneratedProvider(t, true, false) }
func TestGeneratedKeycloakProviderLive(t *testing.T) {
	if os.Getenv("STEGO_REQUIRE_KEYCLOAK_PROVIDER") != "1" {
		t.Skip("real Keycloak provider gate requires CI")
	}
	if os.Getenv("GITHUB_ACTIONS") != "true" {
		t.Fatal("the real Keycloak provider gate runs only in CI")
	}
	testGeneratedProvider(t, true, true)
}
func testGeneratedProvider(t *testing.T, telemetry, live bool) {
	g := new(Generator)
	c := fixture()
	files, wiring, err := g.Generate(c)
	if err != nil {
		t.Fatal(err)
	}
	again, other, err := g.Generate(c)
	if err != nil || !reflect.DeepEqual(files, again) || !reflect.DeepEqual(wiring, other) {
		t.Fatal("unstable provider generation", err)
	}
	tracingImport := ""
	var module strings.Builder
	module.WriteString("module example.com/provider\ngo 1.26.8\n")
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
		keys := make([]string, 0, len(wiring.GoModRequires))
		for key := range wiring.GoModRequires {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		module.WriteString("require (\n")
		for _, key := range keys {
			fmt.Fprintf(&module, "%s %s\n", key, wiring.GoModRequires[key])
		}
		module.WriteString(")\n")
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
	h, err := httpclient.Render("application/client", tracingImport)
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, h)
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() == "trace_test.go" && !telemetry || entry.Name() == "live_test.go" && !telemetry {
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
	if telemetry {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		cmd := exec.CommandContext(ctx, "go", "mod", "tidy")
		cmd.Dir = project
		cmd.Env = append(os.Environ(), "GOWORK=off")
		output, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("provider telemetry dependencies: %v\n%s", err, output)
		}
	}
	budget := 60 * time.Second
	args := []string{"test", "-p=1", "-race", "-count=1", "-timeout=30s", "-v"}
	if live {
		budget = 7 * time.Minute
		args = []string{"test", "-p=1", "-race", "-count=1", "-timeout=6m", "-run", "^TestProviderLive$", "-v"}
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
