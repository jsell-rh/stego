package kubernetesclient

import (
	_ "embed"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/httpapplication"
)

//go:embed testdata/client_test.go
var runtimeTests []byte

func TestGeneratedKubernetesClient(t *testing.T) {
	ctx := gen.Context{ModuleName: "example.com/widget", OutDirName: "out", OutputNamespace: "kubernetes", PeerNamespaces: map[string]string{"http-application": "application", "jwt-auth": "auth"}, StorageContract: "example.com/widget/out/contracts/storage", AuthPackage: "example.com/widget/out/auth", ComponentConfig: map[string]any{"factory_package": "sample"}}
	files, _, err := new(Generator).Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ctx.OutputNamespace = "application"
	httpFiles, _, err := new(httpapplication.Generator).Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range httpFiles {
		if file.Path == "application/client/client.go" {
			files = append(files, file)
		}
	}
	files = append(files, gen.File{Path: "kubernetes/client_test.go", Content: runtimeTests})
	project := t.TempDir()
	for _, file := range files {
		name := filepath.Join(project, "out", file.Path)
		if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, file.Bytes(), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(project, "go.mod"), []byte("module example.com/widget\ngo 1.26.8\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "test", "-race", "-count=1", "-timeout=30s", "./...")
	cmd.Dir = project
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated Kubernetes client: %v\n%s", err, output)
	}
}
func TestMissingHTTPClientIsRejected(t *testing.T) {
	if _, _, err := new(Generator).Generate(gen.Context{ModuleName: "example.com/widget", OutputNamespace: "kubernetes"}); err == nil {
		t.Fatal("missing transport accepted")
	}
}
