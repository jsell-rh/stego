package kubernetesservice

import (
	"bytes"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
)

func rpcContext() gen.Context {
	c := workerContext()
	c.PeerNamespaces["grpc-application"] = "rpcapi"
	c.PeerConfigs = map[string]map[string]any{"grpc-application": {"processes": []any{map[string]any{"name": "records", "factory_package": "internal/records"}}}}
	c.ComponentConfig["rpc_processes"] = []any{map[string]any{"name": "records", "component": "grpc-application", "process": "records", "network_peers": []any{map[string]any{"direction": "ingress", "namespace": "self", "pod_label": "app", "pod_value": "client", "port": 9090, "protocol": "TCP"}}}}
	return c
}

func TestRPCDeploymentValidation(t *testing.T) {
	cases := map[string]func(*gen.Context, map[string]any){
		"missing component":    func(c *gen.Context, p map[string]any) { delete(c.PeerNamespaces, "grpc-application") },
		"missing declarations": func(c *gen.Context, p map[string]any) { c.PeerConfigs = nil },
		"unknown process":      func(c *gen.Context, p map[string]any) { p["process"] = "unknown" },
		"wrong component":      func(c *gen.Context, p map[string]any) { p["component"] = "rest-api" },
		"source outside image": func(c *gen.Context, p map[string]any) { c.ComponentConfig["source_directories"] = nil },
		"process traversal":    func(c *gen.Context, p map[string]any) { p["process"] = "../records" },
		"name traversal":       func(c *gen.Context, p map[string]any) { p["name"] = "../records" },
		"worker collision":     func(c *gen.Context, p map[string]any) { p["name"] = "queue" },
		"duplicate":            func(c *gen.Context, p map[string]any) { c.ComponentConfig["rpc_processes"] = []any{p, p} },
		"wrong port":           func(c *gen.Context, p map[string]any) { p["network_peers"].([]any)[0].(map[string]any)["port"] = 8443 },
		"unknown":              func(c *gen.Context, p map[string]any) { p["privileged"] = true },
		"invalid secret":       func(c *gen.Context, p map[string]any) { p["files_secret"] = "../files" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			c := rpcContext()
			p := c.ComponentConfig["rpc_processes"].([]any)[0].(map[string]any)
			change(&c, p)
			if _, _, err := new(Generator).Generate(c); err == nil {
				t.Fatal("invalid RPC deployment accepted")
			}
		})
	}
}

func TestRPCDeploymentFiles(t *testing.T) {
	c := rpcContext()
	files, _, err := new(Generator).Generate(c)
	if err != nil {
		t.Fatal(err)
	}
	repeat, _, err := new(Generator).Generate(c)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for i, file := range files {
		if file.Path != repeat[i].Path || !bytes.Equal(file.Bytes(), repeat[i].Bytes()) {
			t.Fatal("unstable RPC deployment")
		}
		if file.Path == "deploy/rpc/records/Containerfile" {
			found++
			if !strings.Contains(string(file.Content), "./out/rpcapi/processes/records") || !strings.Contains(string(file.Content), `ENTRYPOINT ["/rpc"]`) {
				t.Fatal("RPC image does not build the declared process")
			}
		}
		if file.Path == "deploy/render/rpc-records.json.tmpl" {
			found++
			for _, value := range []string{"widget-records", "--stego-probe=ready", "--stego-probe=live", "127.0.0.1:9082", "0.0.0.0:9090", "/var/run/stego/tls.key"} {
				if !strings.Contains(string(file.Content), value) {
					t.Fatal("RPC manifest is incomplete", value)
				}
			}
		}
	}
	if found != 2 {
		t.Fatal("RPC deployment files missing")
	}
}
