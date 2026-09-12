package kubernetesservice

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"text/template"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/httpapplication"
	"github.com/jsell-rh/stego/internal/generator/kubernetesclient"
)

func allocationContext() gen.Context {
	c := workerContext()
	c.PeerNamespaces["kubernetes-client"] = "kubernetes"
	c.PeerNamespaces["http-application"] = "application"
	w := c.ComponentConfig["workers"].([]any)[0].(map[string]any)
	w["namespace_allocator"] = true
	w["kubernetes_api"] = true
	w["external_endpoints"] = []any{"kubernetes"}
	c.ComponentConfig["allocation_roles"] = []any{
		object{"name": "data", "scope": "namespace", "rules": []any{object{"api_group": "", "resources": []any{"secrets"}, "verbs": []any{"get"}}}},
		object{"name": "review", "scope": "cluster", "rules": []any{object{"api_group": "authentication.k8s.io", "resources": []any{"tokenreviews"}, "verbs": []any{"create"}}}},
	}
	quota := []any{}
	for _, pair := range [][2]string{{"pods", "1"}, {"limits.cpu", "1"}, {"limits.memory", "256Mi"}, {"limits.ephemeral-storage", "128Mi"}, {"requests.storage", "1Gi"}} {
		quota = append(quota, object{"resource": pair[0], "value": pair[1]})
	}
	c.ComponentConfig["allocation_profiles"] = []any{object{"name": "tenant", "namespace_prefix": "tenant-", "suffix_length": 8, "owner_label": "example.test/owner", "manager": "widget", "quota": quota, "bindings": []any{object{"role": "data", "service_account": "widget-data", "namespace": "control"}, object{"role": "review", "service_account": "gateway", "namespace": "allocated"}}}}
	return c
}
func TestAllocationValidation(t *testing.T) {
	cases := map[string]func(*gen.Context, object, object){
		"no profiles": func(c *gen.Context, p, w object) {
			delete(c.ComponentConfig, "allocation_profiles")
			delete(c.ComponentConfig, "allocation_roles")
		},
		"no worker":                   func(c *gen.Context, p, w object) { delete(w, "namespace_allocator") },
		"no client":                   func(c *gen.Context, p, w object) { delete(c.PeerNamespaces, "kubernetes-client") },
		"no identity":                 func(c *gen.Context, p, w object) { delete(w, "kubernetes_api") },
		"extra allocator permissions": func(c *gen.Context, p, w object) { w["kubernetes_permissions"] = []any{} },
		"template prefix":             func(c *gen.Context, p, w object) { p["namespace_prefix"] = "{{.Namespace}}" },
		"short prefix":                func(c *gen.Context, p, w object) { p["namespace_prefix"] = "-" },
		"invalid suffix":              func(c *gen.Context, p, w object) { p["suffix_length"] = 0 },
		"large suffix":                func(c *gen.Context, p, w object) { p["suffix_length"] = 64 },
		"owner exceeds client limit": func(c *gen.Context, p, w object) {
			p["owner_label"] = strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 61) + "/id"
		},
		"reserved owner":  func(c *gen.Context, p, w object) { p["owner_label"] = "stego.dev/allocator" },
		"security owner":  func(c *gen.Context, p, w object) { p["owner_label"] = "pod-security.kubernetes.io/warn" },
		"missing bounds":  func(c *gen.Context, p, w object) { p["quota"] = []any{} },
		"zero bound":      func(c *gen.Context, p, w object) { p["quota"].([]any)[0].(object)["value"] = "0" },
		"repeated bound":  func(c *gen.Context, p, w object) { p["quota"].([]any)[1] = p["quota"].([]any)[0] },
		"wrong role":      func(c *gen.Context, p, w object) { p["bindings"].([]any)[0].(object)["role"] = "unknown" },
		"wrong namespace": func(c *gen.Context, p, w object) { p["bindings"].([]any)[0].(object)["namespace"] = "other" },
		"both role types": func(c *gen.Context, p, w object) { p["bindings"].([]any)[0].(object)["external_role"] = "external" },
		"allocator data access": func(c *gen.Context, p, w object) {
			p["bindings"].([]any)[0].(object)["service_account"] = "widget-queue"
		},
		"reserved role": func(c *gen.Context, p, w object) {
			c.ComponentConfig["allocation_roles"].([]any)[0].(object)["name"] = "proof"
		},
		"CPU byte units":          func(c *gen.Context, p, w object) { p["quota"].([]any)[1].(object)["value"] = "1Gi" },
		"memory fractional bytes": func(c *gen.Context, p, w object) { p["quota"].([]any)[2].(object)["value"] = "1m" },
		"global data access": func(c *gen.Context, p, w object) {
			c.ComponentConfig["allocation_roles"].([]any)[0].(object)["scope"] = "cluster"
		},
		"global control subject": func(c *gen.Context, p, w object) { p["bindings"].([]any)[1].(object)["namespace"] = "control" },
		"escalation": func(c *gen.Context, p, w object) {
			c.ComponentConfig["allocation_roles"].([]any)[0].(object)["rules"].([]any)[0].(object)["verbs"] = []any{"escalate"}
		},
		"overlap": func(c *gen.Context, p, w object) {
			other := object{}
			for k, v := range p {
				other[k] = v
			}
			other["name"] = "other"
			other["namespace_prefix"] = "tenant-a" + "-"
			other["suffix_length"] = 6
			c.ComponentConfig["allocation_profiles"] = append(c.ComponentConfig["allocation_profiles"].([]any), other)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := allocationContext()
			p := c.ComponentConfig["allocation_profiles"].([]any)[0].(object)
			w := c.ComponentConfig["workers"].([]any)[0].(object)
			mutate(&c, p, w)
			if _, _, err := new(Generator).Generate(c); err == nil {
				t.Fatal("invalid allocation accepted")
			}
		})
	}
}

func TestAllocationManifests(t *testing.T) {
	c := allocationContext()
	if os.Getenv("STEGO_ALLOCATION_NEXT") == "1" {
		p := c.ComponentConfig["allocation_profiles"].([]any)[0].(object)
		p["bindings"] = p["bindings"].([]any)[:1]
		p["bindings"].([]any)[0].(object)["service_account"] = "replacement"
	}
	files, _, err := new(Generator).Generate(c)
	if err != nil {
		t.Fatal(err)
	}
	repeat, _, err := new(Generator).Generate(c)
	if err != nil {
		t.Fatal(err)
	}
	ns := os.Getenv("STEGO_ALLOCATION_NAMESPACE")
	if ns == "" {
		ns = "allocation-check"
	}
	digest := sha256.Sum256([]byte(ns + ".widget-queue"))
	marker := hex.EncodeToString(digest[:16])
	found := false
	for i, f := range files {
		if f.Path != repeat[i].Path || !bytes.Equal(f.Content, repeat[i].Content) {
			t.Fatal("unstable allocation output")
		}
		if f.Path != "deploy/render/worker-queue.json.tmpl" {
			continue
		}
		found = true
		tmpl, err := template.New("manifest").Funcs(template.FuncMap{"allocationID": func(namespace, service string) string {
			if namespace != ns || service != "widget-queue" {
				t.Fatal("wrong allocation identity")
			}
			return marker
		}}).Parse(string(f.Content))
		if err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		if err := tmpl.Execute(&b, struct {
			Namespace, Image string
			FSGroup          int
		}{ns, "registry.test/widget@sha256:" + strings.Repeat("a", 64), 10001}); err != nil {
			t.Fatal(err)
		}
		var doc struct{ Items []object }
		if err := json.Unmarshal(b.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		policies, bindings := 0, 0
		for _, item := range doc.Items {
			switch item["kind"] {
			case "ValidatingAdmissionPolicy":
				policies++
				if item["spec"].(object)["failurePolicy"] != "Fail" {
					t.Fatal("admission fails open")
				}
			case "ClusterRoleBinding":
				bindings++
				if item["roleRef"].(object)["name"] != ns+".widget-queue" {
					t.Fatal("data role bound globally")
				}
			}
		}
		if policies != 3 || bindings != 1 {
			t.Fatal("wrong allocation guards")
		}
		if dir := os.Getenv("STEGO_ALLOCATION_ARTIFACTS"); dir != "" {
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "manifest.json"), b.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !found {
		t.Fatal("allocator deployment missing")
	}
}

//go:embed testdata/allocation_test.go
var allocationRuntimeTests []byte

func TestGeneratedAllocationRuntime(t *testing.T) {
	c := allocationContext()
	files, err := allocationFiles(c)
	if err != nil {
		t.Fatal(err)
	}
	c.OutputNamespace = "kubernetes"
	kube, _, err := new(kubernetesclient.Generator).Generate(c)
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, kube...)
	c.OutputNamespace = "application"
	c.PeerNamespaces["jwt-auth"] = "auth"
	c.AuthPackage = "example.com/widget/out/auth"
	c.StorageContract = "example.com/widget/out/contracts/storage"
	c.ComponentConfig = map[string]any{"factory_package": "sample"}
	httpFiles, _, err := new(httpapplication.Generator).Generate(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range httpFiles {
		if f.Path == "application/client/client.go" {
			files = append(files, f)
		}
	}
	files = append(files, gen.File{Path: "deploy/allocation/allocation_test.go", Content: allocationRuntimeTests})
	dir := t.TempDir()
	for _, f := range files {
		name := filepath.Join(dir, "out", f.Path)
		if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, f.Bytes(), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/widget\ngo 1.26.8\n"), 0644); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("go", "test", "-v", "-race", "-count=1", "-timeout=3m", "./...")
	command.Dir = dir
	command.Env = append(os.Environ(), "GOWORK=off")
	output, err := command.CombinedOutput()
	if dir := os.Getenv("STEGO_ALLOCATION_ARTIFACTS"); dir != "" {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "runtime.log"), output, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err != nil {
		t.Fatalf("generated allocation: %v\n%s", err, output)
	}
}
