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
	return gen.Context{ModuleName: "example.com/widget", GoVersion: "1.26.8", ServiceName: "widget", OutDirName: "out", OutputNamespace: "deploy", PeerNamespaces: map[string]string{"rest-api": "api", "health-check": "health"}, ComponentConfig: map[string]any{"source_directories": []any{"internal"}, "network_peers": []any{map[string]any{"direction": "ingress", "namespace": "self", "pod_label": "app.kubernetes.io/name", "pod_value": "client", "port": 8443, "protocol": "TCP"}}}}
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
		"invalid label prefix": func(c *gen.Context) {
			c.ComponentConfig["network_peers"].([]any)[0].(map[string]any)["pod_label"] = "Bad..prefix/name"
		},
		"invalid label path": func(c *gen.Context) {
			c.ComponentConfig["network_peers"].([]any)[0].(map[string]any)["pod_label"] = "example.test/name/extra"
		},
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
	ctx := workerContext()
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
	for name, source := range ctx.Inputs {
		target := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, source, 0644); err != nil {
			t.Fatal(err)
		}
	}
	runtimePath := filepath.Join(dir, "out/controller")
	if err := os.MkdirAll(runtimePath, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runtimePath, "process.go"), []byte("package controller\nimport \"context\"\ntype Metrics struct{}\nfunc Main(func(context.Context,*Metrics)error){}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	check := `package main
import("bytes";"encoding/json";"strings";"testing")
func TestRenderedPolicy(t *testing.T){
 args:=[]string{"--image","registry.example.test/team/widget@sha256:"+strings.Repeat("a",64),"--namespace","test","--fs-group","10001"}
 var output bytes.Buffer;if err:=render(args,&output);err!=nil{t.Fatal(err)}
 var doc struct{Items []map[string]any};if err:=json.Unmarshal(output.Bytes(),&doc);err!=nil{t.Fatal(err)};if len(doc.Items)!=4{t.Fatal("resource set differs")}
 var deployment map[string]any;for _,item:=range doc.Items{if item["kind"]=="Deployment"{deployment=item}}
 if doc.Items[1]["kind"]!="NetworkPolicy"{t.Fatal("policy must precede deployment")}
 spec:=deployment["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
 if spec["automountServiceAccountToken"]!=false{t.Fatal("token mounted")};security:=spec["securityContext"].(map[string]any);if security["runAsNonRoot"]!=true||security["fsGroup"]!=float64(10001){t.Fatal("pod security differs")}
 c:=spec["containers"].([]any)[0].(map[string]any);if c["securityContext"].(map[string]any)["readOnlyRootFilesystem"]!=true{t.Fatal("writable root")}
 for _,name:=range []string{"startupProbe","livenessProbe","readinessProbe"}{if c[name].(map[string]any)["httpGet"].(map[string]any)["scheme"]!="HTTPS"{t.Fatal("plaintext probe")}}
 output.Reset();if err:=render(append(args,"--worker","queue"),&output);err!=nil{t.Fatal(err)}
 if err:=json.Unmarshal(output.Bytes(),&doc);err!=nil{t.Fatal(err)};if len(doc.Items)!=3{t.Fatal("worker resource set differs")}
 for _,item:=range doc.Items{switch item["kind"]{
 case "Deployment":
  spec:=item["spec"].(map[string]any);if spec["replicas"]!=float64(1)||spec["strategy"].(map[string]any)["type"]!="Recreate"{t.Fatal("worker overlap")}
  pod:=spec["template"].(map[string]any)["spec"].(map[string]any);if pod["automountServiceAccountToken"]!=false{t.Fatal("worker token mount")}
  c:=pod["containers"].([]any)[0].(map[string]any);if _,exists:=c["ports"];exists{t.Fatal("worker has an ingress port")}
  command:=c["readinessProbe"].(map[string]any)["exec"].(map[string]any)["command"].([]any);if len(command)!=2||command[0]!="/worker"||command[1]!="--stego-probe=ready"{t.Fatal("wrong worker probe")}
 case "NetworkPolicy":if len(item["spec"].(map[string]any)["ingress"].([]any))!=0{t.Fatal("worker ingress permitted")}
 }}
 for _,args:=range [][]string{append(args,"--worker","missing"),append(args,"--worker","../queue"),nil,{"--image","widget:latest","--namespace","test"},{"--image","registry.test/widget@sha256:"+strings.Repeat("a",64),"--namespace","../bad"},{"--image","registry.test/widget@sha256:"+strings.Repeat("a",64),"--namespace","test","--fs-group","0"}}{output.Reset();if err:=render(args,&output);err==nil||output.Len()!=0{t.Fatal("invalid input emitted output")}}
}
`
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/widget\n\ngo 1.26.8\n"), 0644); err != nil {
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

func workerContext() gen.Context {
	c := serviceContext()
	c.PeerNamespaces["controller"] = "controller"
	c.ComponentConfig["workers"] = []any{map[string]any{"name": "queue", "package": "internal/task", "function": "Run"}}
	c.Inputs = map[string][]byte{"internal/task/worker.go": []byte(`package task
import("context"; engine "example.com/widget/out/controller")
func Run(ctx context.Context, metrics *engine.Metrics) error { return nil }
`)}
	return c
}

func TestWorkerValidation(t *testing.T) {
	cases := map[string]func(*gen.Context, map[string]any){
		"missing source":    func(c *gen.Context, w map[string]any) { c.Inputs = nil },
		"missing component": func(c *gen.Context, w map[string]any) { delete(c.PeerNamespaces, "controller") },
		"name collision":    func(c *gen.Context, w map[string]any) { c.ComponentConfig["workers"] = []any{w, w} },
		"long name":         func(c *gen.Context, w map[string]any) { w["name"] = strings.Repeat("a", 50) },
		"traversal":         func(c *gen.Context, w map[string]any) { w["package"] = "internal/../escape" },
		"generated source":  func(c *gen.Context, w map[string]any) { w["package"] = "out/source" },
		"undeclared source": func(c *gen.Context, w map[string]any) { w["package"] = "outside/task" },
		"unexported":        func(c *gen.Context, w map[string]any) { w["function"] = "run" },
		"missing function":  func(c *gen.Context, w map[string]any) { w["function"] = "Missing" },
		"wrong metrics": func(c *gen.Context, w map[string]any) {
			c.Inputs["internal/task/worker.go"] = []byte(strings.Replace(string(c.Inputs["internal/task/worker.go"]), "*engine.Metrics", "string", 1))
		},
		"wrong package": func(c *gen.Context, w map[string]any) {
			c.Inputs["internal/task/worker.go"] = []byte(strings.Replace(string(c.Inputs["internal/task/worker.go"]), "package task", "package main", 1))
		},
		"build constraint": func(c *gen.Context, w map[string]any) {
			c.Inputs["internal/task/worker.go"] = append([]byte("//go:build ignore\n\n"), c.Inputs["internal/task/worker.go"]...)
		},
		"unknown field":  func(c *gen.Context, w map[string]any) { w["command"] = "unsafe" },
		"invalid secret": func(c *gen.Context, w map[string]any) { w["files_secret"] = "../outside" },
		"ingress":        func(c *gen.Context, w map[string]any) { w["network_peers"] = c.ComponentConfig["network_peers"] },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := workerContext()
			w := c.ComponentConfig["workers"].([]any)[0].(map[string]any)
			mutate(&c, w)
			if _, _, err := new(Generator).Generate(c); err == nil {
				t.Fatal("invalid worker accepted")
			}
		})
	}
	c := workerContext()
	names, err := new(Generator).InputFiles(c.ComponentConfig)
	if err != nil || len(names) != 1 || names[0] != "internal/task/worker.go" {
		t.Fatal("wrong compiler input", names, err)
	}
}
