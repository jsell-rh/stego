package kubernetesworkload

import (
	"bytes"
	_ "embed"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
)

//go:embed testdata/workload_test.go
var runtimeTests []byte

func TestWorkloadNamespaceAndConfiguration(t *testing.T) {
	for _, ctx := range []gen.Context{{OutputNamespace: "../escape"}, {OutputNamespace: "workload", ComponentConfig: map[string]any{"security_override": true}}} {
		files, wiring, err := new(Generator).Generate(ctx)
		if err == nil || files != nil || wiring != nil {
			t.Fatal("invalid declaration produced files")
		}
	}
}
func TestGeneratedWorkloadConstruction(t *testing.T) {
	ctx := gen.Context{OutputNamespace: "workload"}
	files, _, err := new(Generator).Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	repeat, _, err := new(Generator).Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || len(repeat) != 1 || files[0].Path != "workload/workload.go" || !bytes.Equal(files[0].Bytes(), repeat[0].Bytes()) {
		t.Fatal("generation differs")
	}
	project := t.TempDir()
	if err := os.MkdirAll(filepath.Join(project, "workload"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"go.mod": []byte("module example.com/widget\ngo 1.26.8\nrequire k8s.io/api v0.35.0\nrequire k8s.io/apimachinery v0.35.0\n"), files[0].Path: files[0].Bytes(), "workload/workload_test.go": runtimeTests} {
		if err := os.WriteFile(filepath.Join(project, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	deps := exec.Command("go", "mod", "tidy")
	deps.Dir = project
	deps.Env = append(os.Environ(), "GOWORK=off", "GOMAXPROCS=2")
	if output, err := deps.CombinedOutput(); err != nil {
		t.Fatalf("API test dependencies: %v\n%s", err, output)
	}
	cmd := exec.Command("go", "test", "-json", "-race", "-count=1", "-mod=readonly", "-timeout=60s", "./...")
	cmd.Dir = project
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOMAXPROCS=2")
	output, err := cmd.CombinedOutput()
	t.Log(string(output))
	if err != nil {
		t.Fatalf("generated workload check: %v", err)
	}
}
