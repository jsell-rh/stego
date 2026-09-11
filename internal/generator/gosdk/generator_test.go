package gosdk

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/oteltracing"
)

const sample = `openapi: 3.0.3
info: {title: Sample, version: '1'}
paths:
  /widgets/{id}:
    patch:
      operationId: patchWidget
      parameters:
        - {name: id, in: path, required: true, schema: {type: string}}
      requestBody:
        required: true
        content:
          application/json:
            schema: {$ref: '#/components/schemas/Widget'}
      responses:
        '200':
          description: Widget
          content:
            application/json:
              schema: {$ref: '#/components/schemas/Widget'}
    get:
      operationId: getWidget
      parameters:
        - {name: id, in: path, required: true, schema: {type: string}}
      responses:
        '200':
          description: Widget
          content:
            application/json:
              schema: {$ref: '#/components/schemas/Widget'}
        '404': {description: Missing}
  /uuid/{id}:
    get:
      operationId: getWithBodyUUID
      parameters:
        - {name: id, in: path, required: true, schema: {type: string, format: uuid}}
      responses:
        '204': {description: Empty}
components:
  schemas:
    Widget:
      x-sensitive: true
      type: object
      required: [id, name]
      properties:
        id: {type: string}
        name: {type: string}
        description: {type: string, nullable: true}
        enabled: {type: boolean, nullable: true}
        count: {type: integer, nullable: true}
        updated: {type: string, format: date-time, nullable: true}
        tags: {type: array, nullable: true, items: {type: string}}
`

func fixture() gen.Context {
	return gen.Context{ModuleName: "example.com/sdkprobe", OutputNamespace: "sdk", ComponentConfig: map[string]any{"document": "api.yaml"}, Inputs: map[string][]byte{"api.yaml": []byte(sample)}}
}
func TestInputBoundary(t *testing.T) {
	for _, mutation := range []struct {
		name   string
		change func(*gen.Context)
	}{
		{"remote", func(c *gen.Context) {
			c.Inputs["api.yaml"] = []byte(strings.Replace(sample, "#/components/schemas/Widget", "https://invalid.example/schema", 1))
		}},
		{"undeclared", func(c *gen.Context) {
			c.Inputs["api.yaml"] = []byte(strings.Replace(sample, "#/components/schemas/Widget", "other.yaml#/components/schemas/Widget", 1))
		}},
		{"duplicate", func(c *gen.Context) { c.Inputs["api.yaml"] = append([]byte(sample), []byte("openapi: 3.0.3\n")...) }},
		{"extension", func(c *gen.Context) { c.Inputs["api.yaml"] = append([]byte(sample), []byte("x-go-type: Unsafe\n")...) }},
		{"missing", func(c *gen.Context) { delete(c.Inputs, "api.yaml") }},
		{"cycle", func(c *gen.Context) {
			c.Inputs["api.yaml"] = []byte(strings.Replace(sample, "name: {type: string}", "name: {$ref: '#/components/schemas/Widget'}", 1))
		}},
		{"unused callback", func(c *gen.Context) { c.Inputs["api.yaml"] = []byte(sample + "  callbacks:\n    Unused: {}\n") }},
		{"missing paths", func(c *gen.Context) {
			c.Inputs["api.yaml"] = []byte("openapi: 3.0.3\ninfo: {title: Empty, version: '1'}\n")
		}},
		{"missing responses", func(c *gen.Context) {
			c.Inputs["api.yaml"] = []byte("openapi: 3.0.3\ninfo: {title: Empty, version: '1'}\npaths:\n  /widgets:\n    get: {operationId: getWidgets}\n")
		}},
		{"unknown setting", func(c *gen.Context) { c.ComponentConfig["documents"] = "api.yaml" }},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			c := fixture()
			mutation.change(&c)
			g := new(Generator)
			if err := g.ValidateContext(c); err == nil {
				t.Fatal("invalid SDK contract passed preflight")
			}
			files, _, err := g.Generate(c)
			if err == nil || len(files) != 0 {
				t.Fatal("invalid SDK contract produced files")
			}
		})
	}
}
func TestGeneratedSDK(t *testing.T) {
	for _, traced := range []bool{false, true} {
		t.Run(fmt.Sprint(traced), func(t *testing.T) { testGeneratedSDK(t, traced) })
	}
}
func testGeneratedSDK(t *testing.T, traced bool) {
	ctx := fixture()
	if traced {
		ctx.PeerNamespaces = map[string]string{"otel-tracing": "tracing"}
	}

	g := new(Generator)
	files, wiring, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	again, _, err := g.Generate(ctx)
	if err != nil || len(again) != len(files) {
		t.Fatal("repeat generation failed", err)
	}
	for i := range files {
		if files[i].Path != again[i].Path || !bytes.Equal(files[i].Bytes(), again[i].Bytes()) {
			t.Fatal("SDK generation is not deterministic")
		}
	}
	requires := wiring.GoModRequires
	if traced {
		traceFiles, traceWiring, err := new(oteltracing.Generator).Generate(gen.Context{OutputNamespace: "tracing", ServiceName: "sdk-probe"})
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, traceFiles...)
		for name, version := range traceWiring.GoModRequires {
			requires[name] = version
		}
	}
	files = append(files, gen.File{Path: "sdk/client_test.go", Content: []byte(runtimeTest)})
	files = append(files, gen.File{Path: "sdk/nullable_test.go", Content: []byte(nullableRuntimeTest)})
	root := t.TempDir()
	for _, file := range files {
		target := filepath.Join(root, file.Path)
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, file.Bytes(), 0644); err != nil {
			t.Fatal(err)
		}
	}
	var mod strings.Builder
	mod.WriteString("module example.com/sdkprobe\ngo 1.26.0\nrequire(\n")
	names := make([]string, 0, len(requires))
	for name := range requires {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(&mod, "%s %s\n", name, requires[name])
	}
	mod.WriteString(")\n")
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte(mod.String()), 0644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"mod", "tidy"}, {"test", "-race", "-count=1", "-timeout=60s", "./..."}, {"vet", "./..."}} {
		cmd := exec.Command("go", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GOWORK=off")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("SDK compile and runtime: %v %s", err, output)
		}
	}
}

const runtimeTest = `package sdk
import("context";"crypto/tls";"encoding/pem";"errors";"net/http";"net/http/httptest";"os";"path/filepath";"strings";"testing";"time")
func TestHTTPSAndStatus(t *testing.T){
 t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT","")
 server:=httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
  if r.Header.Get("Authorization")!="Bearer private-token"{t.Error("token missing")}
  if r.URL.Path!="/widgets/one"&&r.URL.Path!="/prefix/widgets/one"&&!strings.HasPrefix(r.URL.Path,"/widgets/"){t.Error("unexpected API path")}
  if r.URL.Path=="/widgets/missing"{w.WriteHeader(404);return}
  if r.URL.Path=="/widgets/malformed"{w.Header().Set("Content-Type","application/json");w.Write([]byte("private-token"));return}
  if r.URL.Path=="/widgets/redirect"{http.Redirect(w,r,"https://invalid.example",302);return}
  if r.URL.Path=="/widgets/large"{w.Header().Set("Content-Type","application/json");w.Write([]byte(strings.Repeat("x",(4<<20)+1)));return}
  if r.URL.Path=="/widgets/wait"{<-r.Context().Done();return}
  w.Header().Set("Content-Type","application/json");w.Write([]byte("{\"id\":\"one\",\"name\":\"Widget\"}"))
 }));server.TLS=&tls.Config{MinVersion:tls.VersionTLS13};server.StartTLS();defer server.Close()
 ca:=filepath.Join(t.TempDir(),"ca.pem");os.WriteFile(ca,pem.EncodeToMemory(&pem.Block{Type:"CERTIFICATE",Bytes:server.Certificate().Raw}),0600)
 client,err:=NewClient(Options{BaseURL:server.URL,CAFile:ca,Token:"private-token"});if err!=nil{t.Fatal(err)};defer client.Close()
 got,err:=client.GetWidgetWithResponse(context.Background(),"one");if err!=nil||got.JSON200==nil||got.JSON200.Id!="one"{t.Fatal("typed SDK read failed",err)}
 denied,err:=client.GetWidgetWithResponse(context.Background(),"missing");if err!=nil||denied.StatusCode()!=404{t.Fatal("SDK lost status",err)}
 for _,id:=range []string{"redirect","large","malformed","../private-token"}{if _,err:=client.GetWidgetWithResponse(context.Background(),id);err==nil||strings.Contains(err.Error(),"private-token"){t.Fatal("unsafe request or error",err)}}
 short,cancel:=context.WithTimeout(context.Background(),30*time.Millisecond);defer cancel()
 if _,err:=client.GetWidgetWithResponse(short,"wait");!errors.Is(err,context.DeadlineExceeded){t.Fatal("SDK lost caller deadline",err)}
 if bad,err:=NewClient(Options{BaseURL:strings.Replace(server.URL,"https:","http:",1),Token:"private-token"});err==nil{bad.Close();t.Fatal("SDK accepted plaintext")}
 if bad,err:=NewClient(Options{BaseURL:server.URL,Token:"private-token"});err==nil{defer bad.Close();if _,err:=bad.GetWidgetWithResponse(context.Background(),"one");err==nil{t.Fatal("SDK accepted an untrusted certificate")}}
 prefix,err:=NewClient(Options{BaseURL:server.URL+"/prefix",CAFile:ca,Token:"private-token"});if err!=nil{t.Fatal(err)};defer prefix.Close()
 if _,err:=prefix.GetWidgetWithResponse(context.Background(),"one");err!=nil{t.Fatal("SDK path prefix failed",err)}
 if err:=prefix.acquire(context.Background());err!=nil{t.Fatal(err)}
 start:=time.Now();prefix.Close();if time.Since(start)>2*time.Second{t.Fatal("SDK close did not bound pending caller work")};prefix.release()
 client.Close();if _,err:=client.GetWidgetWithResponse(context.Background(),"one");err==nil{t.Fatal("closed SDK client accepted a request")}
}
`

func TestCapturedReferenceNames(t *testing.T) {
	ctx := fixture()
	ctx.ComponentConfig["references"] = []any{"schemas/api.a.yaml", "schemas/api.b.yaml"}
	ctx.Inputs["api.yaml"] = []byte(strings.Replace(sample, "name: {type: string}", "name: {$ref: 'schemas/api.a.yaml#/components/schemas/Value'}\n        other: {$ref: 'schemas/api.b.yaml#/components/schemas/Value'}", 1))
	ctx.Inputs["schemas/api.a.yaml"] = []byte("components:\n  schemas:\n    Value: {type: string}\n")
	ctx.Inputs["schemas/api.b.yaml"] = []byte("components:\n  schemas:\n    Value: {type: integer}\n")
	g := new(Generator)
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		next, _, err := g.Generate(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for j := range files {
			if !bytes.Equal(files[j].Bytes(), next[j].Bytes()) {
				t.Fatal("reference naming is not deterministic")
			}
		}
	}
	doc, err := loadDocument(ctx)
	if err != nil {
		t.Fatal(err)
	}
	props := doc.Components.Schemas["Widget"].Value.Properties
	if props["name"].Ref == props["other"].Ref || !props["name"].Value.Type.Is("string") || !props["other"].Value.Type.Is("integer") {
		t.Fatal("captured schemas were merged")
	}
}

func TestGeneratedNameConflicts(t *testing.T) {
	for _, source := range []string{
		strings.ReplaceAll(sample, "Widget", "Options"),
		strings.Replace(sample, "name: {type: string}", "foo-bar: {type: string}\n        foo_bar: {type: string}\n        name: {type: string}", 1),
	} {
		ctx := fixture()
		ctx.Inputs["api.yaml"] = []byte(source)
		files, _, err := new(Generator).Generate(ctx)
		if err == nil || len(files) != 0 {
			t.Fatal("conflicting SDK names produced files")
		}
	}
}
