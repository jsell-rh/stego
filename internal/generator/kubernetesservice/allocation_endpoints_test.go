package kubernetesservice

import (
	"bytes"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAllocationEndpointValidation(t *testing.T) {
	for _, value := range []any{nil, true, []any{}, []any{true}, []any{"bad name"}, []any{"api", "api"}, make([]any, 17)} {
		c := allocationContext()
		p := c.ComponentConfig["allocation_profiles"].([]any)[0].(object)
		p["network_isolation"] = true
		p["network_endpoints"] = value
		if _, err := allocationConfig(c); err == nil {
			t.Fatal("invalid endpoint declaration accepted", value)
		}
	}
	c := allocationContext()
	p := c.ComponentConfig["allocation_profiles"].([]any)[0].(object)
	p["network_endpoints"] = []any{"api"}
	if _, err := allocationConfig(c); err == nil {
		t.Fatal("endpoints accepted without isolation")
	}
	p["network_isolation"] = true
	if _, err := allocationConfig(c); err != nil {
		t.Fatal(err)
	}
	worker := c.ComponentConfig["workers"].([]any)[0].(object)
	values := []any{"kubernetes"}
	for i := 0; i < 31; i++ {
		values = append(values, fmt.Sprintf("own-%d", i))
	}
	worker["external_endpoints"] = values
	if _, err := allocationConfig(c); err == nil {
		t.Fatal("unsatisfiable worker and profile endpoint set accepted")
	}
}

//go:embed testdata/allocation_endpoint_renderer_test.go
var allocationEndpointRendererTests []byte

func TestGeneratedAllocationEndpointRenderer(t *testing.T) {
	c := allocationContext()
	p := c.ComponentConfig["allocation_profiles"].([]any)[0].(object)
	p["network_isolation"] = true
	p["network_endpoints"] = []any{"kubernetes", "provider"}
	p["network_peers"] = []any{
		object{"direction": "ingress", "namespace": "control", "pod_label": "app", "pod_value": "controller", "port": 8080, "protocol": "TCP"},
		object{"direction": "egress", "namespace": "allocated", "pod_label": "app", "pod_value": "database", "port": 5432, "protocol": "TCP"},
		object{"direction": "egress", "namespace": "external", "external_namespace": "cluster-dns", "pod_label": "app", "pod_value": "dns", "port": 53, "protocol": "UDP"},
	}
	c.ComponentConfig["workers"] = append(c.ComponentConfig["workers"].([]any), object{"name": "data", "package": "internal/task", "function": "Run", "kubernetes_api": true, "external_endpoints": []any{"kubernetes"}}, object{"name": "other", "package": "internal/task", "function": "Run"})
	files, _, err := new(Generator).Generate(c)
	if err != nil {
		t.Fatal(err)
	}
	repeated, _, err := new(Generator).Generate(c)
	if err != nil || len(files) != len(repeated) {
		t.Fatal("repeat generation failed", err)
	}
	dir := t.TempDir()
	for i, file := range files {
		if file.Path != repeated[i].Path || !bytes.Equal(file.Bytes(), repeated[i].Bytes()) {
			t.Fatal("endpoint generation differs")
		}
		if file.Path != "deploy/resources.go" && !strings.HasPrefix(file.Path, "deploy/render/") {
			continue
		}
		target := filepath.Join(dir, "out", file.Path)
		if err = os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(target, file.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.WriteFile(filepath.Join(dir, "out/deploy/render/endpoints_test.go"), allocationEndpointRendererTests, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/widget\ngo 1.26.8\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "test", "-p=1", "-race", "-count=1", "-mod=readonly", "-timeout=30s", "./out/deploy/render")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated endpoint renderer: %v\n%s", err, output)
	}
}

//go:embed testdata/allocation_endpoint_runtime_test.go
var allocationEndpointRuntimeTests []byte

func TestGeneratedAllocationEndpointRuntime(t *testing.T) {
	testGeneratedAllocationRuntime(t, "^TestAllocationEndpoint")
}
