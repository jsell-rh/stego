package postgresclient

import (
	_ "embed"
	"github.com/jsell-rh/stego/internal/gen"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

//go:embed testdata/client_test.go
var tests []byte

func TestGeneratedPostgresClient(t *testing.T) {
	files, wiring, err := new(Generator).Generate(gen.Context{OutputNamespace: "postgres"})
	if err != nil {
		t.Fatal(err)
	}
	if wiring.GoModRequires["github.com/jackc/pgx/v5"] != "v5.11.0" {
		t.Fatal("missing dependency")
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
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/sql-client\ngo "+new(Generator).MinimumGoVersion()+"\nrequire github.com/jackc/pgx/v5 v5.11.0\n"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"mod", "tidy", "-go=" + new(Generator).MinimumGoVersion()}, {"vet", "-mod=readonly", "./..."}, {"test", "-race", "-count=1", "-mod=readonly", "-timeout=60s", "./..."}} {
		cmd := exec.Command("go", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GOWORK=off")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("generated SQL client: %v\n%s", err, output)
		}
	}
}
