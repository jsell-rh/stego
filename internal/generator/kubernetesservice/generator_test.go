package kubernetesservice

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
)

func serviceContext() gen.Context {
	return gen.Context{ModuleName: "example.com/widget", GoVersion: "1.26.8", ServiceName: "widget", OutDirName: "out", OutputNamespace: "deploy", PeerNamespaces: map[string]string{"rest-api": "api", "health-check": "health"}, ComponentConfig: map[string]any{"source_directories": []any{"internal"}, "network_peers": []any{map[string]any{"direction": "ingress", "namespace": "self", "pod_label": "app", "pod_value": "client", "port": 8443, "protocol": "TCP"}}}}
}

func TestDeploymentValidation(t *testing.T) {
	tests := map[string]func(*gen.Context){
		"service name":     func(c *gen.Context) { c.ServiceName = "../bad" },
		"output":           func(c *gen.Context) { c.OutDirName = "foo/bar" },
		"Go target":        func(c *gen.Context) { c.GoVersion = "1.27.0" },
		"missing probes":   func(c *gen.Context) { delete(c.PeerNamespaces, "health-check") },
		"source traversal": func(c *gen.Context) { c.ComponentConfig["source_directories"] = []any{"../secret"} },
		"source wildcard":  func(c *gen.Context) { c.ComponentConfig["source_directories"] = []any{"*"} },
		"source duplicate": func(c *gen.Context) { c.ComponentConfig["source_directories"] = []any{"out"} },
		"source type":      func(c *gen.Context) { c.ComponentConfig["source_directories"] = []any{42} },
		"unknown":          func(c *gen.Context) { c.ComponentConfig["privileged"] = true },
		"secret":           func(c *gen.Context) { c.ComponentConfig["files_secret"] = "bad\nvalue" },
		"dns":              func(c *gen.Context) { c.ComponentConfig["dns_port"] = 0 },
		"peer extra":       func(c *gen.Context) { c.ComponentConfig["network_peers"].([]any)[0].(map[string]any)["extra"] = true },
		"peer wildcard": func(c *gen.Context) {
			c.ComponentConfig["network_peers"].([]any)[0].(map[string]any)["pod_value"] = "*"
		},
		"peer disabled port": func(c *gen.Context) { c.ComponentConfig["network_peers"].([]any)[0].(map[string]any)["port"] = 9090 },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			ctx := serviceContext()
			mutate(&ctx)
			if _, _, err := (&Generator{}).Generate(ctx); err == nil {
				t.Fatal("invalid deployment accepted")
			}
		})
	}
}

func TestGeneratedDeploymentRenderer(t *testing.T) {
	ctx := serviceContext()
	files, _, err := (&Generator{}).Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	repeat, _, err := (&Generator{}).Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for i, file := range files {
		if file.Path != repeat[i].Path || !bytes.Equal(file.Content, repeat[i].Content) {
			t.Fatal("unstable deployment")
		}
		name := filepath.Join(dir, file.Path)
		if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, file.Bytes(), 0644); err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(name, "Containerfile") && strings.Contains(string(file.Content), "COPY . ") {
			t.Fatal("unbounded build context")
		}
	}
	check := `package main
import("bytes";"encoding/json";"strings";"testing")
func TestRenderedPolicy(t *testing.T){
 args:=[]string{"--image","registry.example.test/team/widget@sha256:"+strings.Repeat("a",64),"--namespace","test","--fs-group","10001"}
 var output bytes.Buffer;if err:=render(args,&output);err!=nil{t.Fatal(err)}
 var doc struct{Items []map[string]any};if err:=json.Unmarshal(output.Bytes(),&doc);err!=nil{t.Fatal(err)};if len(doc.Items)!=4{t.Fatal("resource set differs")}
 deployment:=doc.Items[1];spec:=deployment["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
 if spec["automountServiceAccountToken"]!=false{t.Fatal("token mounted")};security:=spec["securityContext"].(map[string]any);if security["runAsNonRoot"]!=true||security["fsGroup"]!=float64(10001){t.Fatal("pod security differs")}
 c:=spec["containers"].([]any)[0].(map[string]any);if c["securityContext"].(map[string]any)["readOnlyRootFilesystem"]!=true{t.Fatal("writable root")}
 for _,name:=range []string{"startupProbe","livenessProbe","readinessProbe"}{if c[name].(map[string]any)["httpGet"].(map[string]any)["scheme"]!="HTTPS"{t.Fatal("plaintext probe")}}
 for _,args:=range [][]string{nil,{"--image","widget:latest","--namespace","test"},{"--image","registry.test/widget@sha256:"+strings.Repeat("a",64),"--namespace","../bad"},{"--image","registry.test/widget@sha256:"+strings.Repeat("a",64),"--namespace","test","--fs-group","0"}}{output.Reset();if err:=render(args,&output);err==nil||output.Len()!=0{t.Fatal("invalid input emitted output")}}
}
`
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/deployment\n\ngo 1.26.8\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "deploy/render/main_test.go"), []byte(check), 0644); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("go", "test", "-race", "-mod=readonly", "-timeout=20s", "./...")
	command.Dir = dir
	command.Env = append(os.Environ(), "GOWORK=off")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generated deployment: %v\n%s", err, output)
	}
}
