package postgresclient

import (
	"bytes"
	_ "embed"
	"fmt"
	"github.com/jsell-rh/stego/internal/gen"
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

func TestGeneratedPostgresClient(t *testing.T) {
	files, wiring, err := new(Generator).Generate(gen.Context{OutputNamespace: "postgres"})
	if err != nil {
		t.Fatal(err)
	}
	if wiring.GoModRequires["github.com/jackc/pgx/v5"] != "v5.11.0" {
		t.Fatal("missing dependency")
	}
	again, _, err := new(Generator).Generate(gen.Context{OutputNamespace: "postgres"})
	if err != nil || len(again) != len(files) {
		t.Fatal("repeat generation failed", err)
	}
	for i := range files {
		if files[i].Path != again[i].Path || !bytes.Equal(files[i].Bytes(), again[i].Bytes()) {
			t.Fatal("generation changed")
		}
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
	fmt.Fprintf(&mod, "module example.com/sql-client\ngo %s\nrequire (\n", new(Generator).MinimumGoVersion())
	for name, version := range wiring.GoModRequires {
		fmt.Fprintf(&mod, "%s %s\n", name, version)
	}
	mod.WriteString("go.opentelemetry.io/otel/sdk v1.46.0\ngo.opentelemetry.io/otel/sdk/metric v1.46.0\n)\n")
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(mod.String()), 0644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"mod", "tidy", "-go=" + new(Generator).MinimumGoVersion()}, {"vet", "-mod=readonly", "./..."}, {"test", "-v", "-race", "-count=1", "-mod=readonly", "-timeout=120s", "./..."}} {
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
