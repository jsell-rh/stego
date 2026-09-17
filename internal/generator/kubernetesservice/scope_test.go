package kubernetesservice

import (
	"bytes"
	_ "embed"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

//go:embed testdata/render_library_test.go
var deploymentLibraryTests []byte

//go:embed testdata/render_scope_test.go
var deploymentScopeTests []byte

func TestGeneratedDeploymentScopes(t *testing.T) {
	ctx := allocationContext()
	rpc := rpcContext()
	ctx.PeerNamespaces["grpc-application"] = rpc.PeerNamespaces["grpc-application"]
	ctx.PeerConfigs = rpc.PeerConfigs
	ctx.ComponentConfig["rpc_processes"] = rpc.ComponentConfig["rpc_processes"]
	ctx.ComponentConfig["kubernetes_api"] = true
	ctx.ComponentConfig["external_endpoints"] = []any{"kubernetes"}
	ctx.ComponentConfig["kubernetes_permissions"] = []any{
		permission(),
		object{"scope": "cluster", "api_group": "", "resources": []any{"nodes"}, "verbs": []any{"get"}},
	}
	ctx.ComponentConfig["workers"] = append(ctx.ComponentConfig["workers"].([]any), object{
		"name": "metadata", "package": "internal/task", "function": "Run", "kubernetes_api": true,
		"external_endpoints":     []any{"kubernetes"},
		"kubernetes_permissions": []any{object{"scope": "cluster", "api_group": "", "resources": []any{"nodes"}, "verbs": []any{"get"}}},
	})
	files, _, err := new(Generator).Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	repeated, _, err := new(Generator).Generate(ctx)
	if err != nil || len(files) != len(repeated) {
		t.Fatal("repeat generation failed", err)
	}
	dir := t.TempDir()
	for i, file := range files {
		if file.Path != repeated[i].Path || !bytes.Equal(file.Bytes(), repeated[i].Bytes()) {
			t.Fatal("deployment generation differs")
		}
		if file.Path != "deploy/resources.go" && !strings.HasPrefix(file.Path, "deploy/render/") {
			continue
		}
		path := filepath.Join(dir, "out", file.Path)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, file.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "out/deploy/scope_test.go"), deploymentScopeTests, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "out/deploy/library_test.go"), deploymentLibraryTests, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/widget\ngo 1.26.8\n"), 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("go", "test", "-v", "-race", "-count=1", "-mod=readonly", "-timeout=30s", "./out/deploy")
	command.Dir = dir
	command.Env = append(os.Environ(), "GOWORK=off")
	output, commandErr := command.CombinedOutput()
	if evidence := os.Getenv("STEGO_DEPLOYMENT_ARTIFACTS"); evidence != "" {
		if err := os.MkdirAll(evidence, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(evidence, "runtime.log"), output, 0600); err != nil {
			t.Fatal(err)
		}
		generated, err := os.ReadFile(filepath.Join(dir, "out/deploy/resources.go"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(evidence, "generated-renderer.go"), generated, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if commandErr != nil {
		t.Fatalf("generated scope checks: %v\n%s", commandErr, output)
	}
}
