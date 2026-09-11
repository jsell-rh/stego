package postgresadapter

import (
	_ "embed"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
)

//go:embed testdata/database_test.go
var databaseTests []byte

func TestGeneratedDatabaseDriver(t *testing.T) {
	ctx := basicContext()
	ctx.ModuleName, ctx.OutputNamespace = "example.com/dbprobe", "storage"
	ctx.PeerNamespaces = map[string]string{"otel-tracing": "tracing"}
	file, err := generateDatabaseOpener(ctx)
	if err != nil {
		t.Fatal(err)
	}
	project := t.TempDir()
	for _, f := range []gen.File{file, {Path: "storage/database_test.go", Content: databaseTests}, {Path: "tracing/tracing.go", Content: []byte(`package tracing
import("context";"sync")
type Key struct{}
type Record struct { Call, Outcome string; Value any }
var Events=make(chan Record,128)
func TraceDatabase(ctx context.Context,call string)(context.Context,func(string)){
 var once sync.Once
 return ctx,func(outcome string){once.Do(func(){Events<-Record{call,outcome,ctx.Value(Key{})}})}
}
`)}, {Path: "go.mod", Content: []byte("module example.com/dbprobe\ngo 1.25.0\nrequire github.com/jackc/pgx/v5 v5.11.0\n")}} {
		name := filepath.Join(project, f.Path)
		if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, f.Bytes(), 0644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"mod", "tidy"}, {"test", "-race", "-count=1", "-timeout=60s", "./..."}} {
		command := exec.Command("go", args...)
		command.Dir = project
		command.Env = append(os.Environ(), "GOWORK=off")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("database driver: %v %s", err, output)
		}
	}
}

func TestDatabasePeerValidation(t *testing.T) {
	ctx := basicContext()
	ctx.PeerNamespaces = map[string]string{"otel-tracing": "../invalid"}
	if err := new(Generator).ValidateContext(ctx); err == nil {
		t.Fatal("invalid telemetry namespace accepted")
	}
}
