package postgresclient

import (
	"bytes"
	_ "embed"
	"fmt"
	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/oteltracing"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

//go:embed testdata/client_test.go
var tests []byte

//go:embed testdata/provision_test.go
var provisionTests []byte

//go:embed testdata/telemetry_test.go
var telemetryTests []byte

func TestGeneratedPostgresClient(t *testing.T) { testGeneratedPostgresClient(t, "") }
func TestGeneratedClientPrivateTelemetry(t *testing.T) {
	testGeneratedPostgresClient(t, "^TestClientPrivateTelemetry$")
}
func testGeneratedPostgresClient(t *testing.T, pattern string) {
	ctx := gen.Context{ModuleName: "example.com/sql-client", OutputNamespace: "postgres", PeerNamespaces: map[string]string{"otel-tracing": "tracing"}}
	files, wiring, err := new(Generator).Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if wiring.GoModRequires["github.com/jackc/pgx/v5"] != "v5.11.0" {
		t.Fatal("missing dependency")
	}
	again, _, err := new(Generator).Generate(ctx)
	if err != nil || len(again) != len(files) {
		t.Fatal("repeat generation failed", err)
	}
	for i := range files {
		if files[i].Path != again[i].Path || !bytes.Equal(files[i].Bytes(), again[i].Bytes()) {
			t.Fatal("generation changed")
		}
	}
	tracingFiles, tracingWiring, err := new(oteltracing.Generator).Generate(gen.Context{OutputNamespace: "tracing", ServiceName: "sql-client"})
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, tracingFiles...)
	files = append(files, gen.File{Path: "postgres/telemetry_test.go", Content: telemetryTests})
	for name, version := range tracingWiring.GoModRequires {
		wiring.GoModRequires[name] = version
	}
	dir := t.TempDir()
	for _, file := range files {
		name := filepath.Join(dir, file.Path)
		if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, file.Bytes(), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "postgres/client_test.go"), tests, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "postgres/provision_test.go"), provisionTests, 0644); err != nil {
		t.Fatal(err)
	}
	var mod strings.Builder
	fmt.Fprintf(&mod, "module example.com/sql-client\ngo %s\nrequire (\n", new(oteltracing.Generator).MinimumGoVersion())
	for name, version := range wiring.GoModRequires {
		fmt.Fprintf(&mod, "%s %s\n", name, version)
	}
	mod.WriteString(")\n")
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(mod.String()), 0644); err != nil {
		t.Fatal(err)
	}
	commands := [][]string{{"mod", "tidy", "-go=" + new(oteltracing.Generator).MinimumGoVersion()}}
	if pattern == "" {
		commands = append(commands, []string{"vet", "-mod=readonly", "./..."})
	}
	args := []string{"test", "-v", "-race", "-count=1", "-mod=readonly", "-timeout=120s"}
	if pattern != "" {
		args = append(args, "-run", pattern)
	}
	commands = append(commands, append(args, "./..."))
	for _, args := range commands {
		cmd := exec.Command("go", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GOWORK=off")
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("generated SQL client: %v\n%s", err, output)
		}
		if args[0] == "test" {
			t.Logf("%s", output)
		}
	}
}

func TestTracingPeer(t *testing.T) {
	for _, peer := range []string{"", "tracing", "../escape"} {
		ctx := gen.Context{ModuleName: "example.com/records", OutDirName: "out", OutputNamespace: "postgres", PeerNamespaces: map[string]string{"otel-tracing": peer}}
		files, _, err := new(Generator).Generate(ctx)
		if peer == "../escape" {
			if err == nil || len(files) != 0 {
				t.Fatal("invalid telemetry namespace was accepted")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		var source string
		for _, file := range files {
			source += string(file.Bytes())
		}
		if strings.Contains(source, "otel.Tracer") || strings.Contains(source, "otel.Meter") || strings.Contains(source, "slog.") {
			t.Fatal("client uses global telemetry")
		}
		if strings.Contains(source, "telemetry.TracePostgresOperation") != (peer != "") {
			t.Fatal("client does not use its declared telemetry peer")
		}
		if peer != "" && !strings.Contains(source, `"example.com/records/out/tracing"`) {
			t.Fatal("client lost the peer module path")
		}
	}
}
