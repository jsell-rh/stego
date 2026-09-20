package kubernetesworkload

import (
	"bytes"
	_ "embed"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/httpapplication"
	"github.com/jsell-rh/stego/internal/generator/kubernetesclient"
)

//go:embed testdata/workload_test.go
var runtimeTests []byte

//go:embed testdata/dependency_data_test.go
var dependencyDataTests []byte

//go:embed testdata/updates_test.go
var updateTests []byte

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
	contents := map[string][]byte{"go.mod": []byte("module example.com/widget\ngo 1.26.8\nrequire k8s.io/api v0.35.0\nrequire k8s.io/apimachinery v0.35.0\n"), files[0].Path: files[0].Bytes(), "workload/workload_test.go": runtimeTests, "workload/updates_test.go": updateTests, "workload/dependency_data_test.go": dependencyDataTests}
	clientContext := gen.Context{ModuleName: "example.com/widget", OutputNamespace: "kubernetes", PeerNamespaces: map[string]string{"http-application": "application", "jwt-auth": "auth"}, StorageContract: "example.com/widget/contracts/storage", AuthPackage: "example.com/widget/auth", ComponentConfig: map[string]any{"factory_package": "sample"}}
	clientFiles, _, err := new(kubernetesclient.Generator).Generate(clientContext)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range clientFiles {
		contents[file.Path] = file.Bytes()
	}
	clientContext.OutputNamespace = "application"
	httpFiles, _, err := new(httpapplication.Generator).Generate(clientContext)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range httpFiles {
		if file.Path == "application/client/client.go" {
			contents[file.Path] = file.Bytes()
		}
	}
	for name, data := range contents {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(project, name)), 0700); err != nil {
			t.Fatal(err)
		}
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
	cmd := exec.Command("go", "test", "-json", "-race", "-count=1", "-mod=readonly", "-timeout=60s", "./workload")
	cmd.Dir = project
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOMAXPROCS=2")
	output, err := cmd.CombinedOutput()
	t.Log(string(output))
	if err != nil {
		t.Fatalf("generated workload check: %v", err)
	}
}
