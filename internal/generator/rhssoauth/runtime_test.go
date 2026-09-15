package rhssoauth

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/jsell-rh/stego/internal/gen"
)

func TestGeneratedSSOTrustPolicy(t *testing.T) {
	project := t.TempDir()
	files, wiring, err := new(Generator).Generate(gen.Context{OutputNamespace: "auth"})
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		target := filepath.Join(project, file.Path)
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, file.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	}
	tests, err := os.ReadFile("testdata/trust_test.go")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "auth/trust_test.go"), tests, 0600); err != nil {
		t.Fatal(err)
	}
	module := "module example.com/sso-test\ngo 1.26.8\n"
	for name, version := range wiring.GoModRequires {
		module += fmt.Sprintf("require %s %s\n", name, version)
	}
	if err := os.WriteFile(filepath.Join(project, "go.mod"), []byte(module), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	for _, args := range [][]string{{"mod", "tidy"}, {"test", "-p=1", "-race", "-count=1", "-mod=readonly", "-timeout=45s", "./..."}} {
		command := exec.CommandContext(ctx, "go", args...)
		command.Dir = project
		command.Env = append(os.Environ(), "GOWORK=off", "GOMAXPROCS=1", "GOMEMLIMIT=384MiB")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("SSO runtime: %v\n%s", err, output)
		}
	}
}
