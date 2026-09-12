package kubernetesservice

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
)

func permission() map[string]any {
	return map[string]any{"scope": "namespace", "api_group": "", "resources": []any{"configmaps"}, "verbs": []any{"get"}, "resource_names": []any{"settings"}}
}
func accessConfig() map[string]any {
	return map[string]any{"kubernetes_api": true, "external_endpoints": []any{"kubernetes"}, "kubernetes_permissions": []any{permission()}}
}

func TestKubernetesAccessValidation(t *testing.T) {
	cases := map[string]func(map[string]any, map[string]any){
		"string opt in":                func(c, p map[string]any) { c["kubernetes_api"] = "true" },
		"permissions without identity": func(c, p map[string]any) { c["kubernetes_api"] = false },
		"undeclared egress":            func(c, p map[string]any) { delete(c, "external_endpoints") },
		"unknown scope":                func(c, p map[string]any) { p["scope"] = "all" },
		"wildcard group":               func(c, p map[string]any) { p["api_group"] = "*" },
		"wildcard resource":            func(c, p map[string]any) { p["resources"] = []any{"*"} },
		"wildcard verb":                func(c, p map[string]any) { p["verbs"] = []any{"*"} },
		"escalation":                   func(c, p map[string]any) { p["verbs"] = []any{"escalate"} },
		"impersonation":                func(c, p map[string]any) { p["verbs"] = []any{"impersonate"} },
		"bind":                         func(c, p map[string]any) { p["verbs"] = []any{"bind"} },
		"delete collection":            func(c, p map[string]any) { p["verbs"] = []any{"deletecollection"} },
		"name restricted create":       func(c, p map[string]any) { p["verbs"] = []any{"create"} },
		"template name":                func(c, p map[string]any) { p["resource_names"] = []any{"{{.Namespace}}"} },
		"duplicate verb":               func(c, p map[string]any) { p["verbs"] = []any{"get", "get"} },
		"unknown field":                func(c, p map[string]any) { p["nonResourceURLs"] = []any{"/"} },
		"empty names":                  func(c, p map[string]any) { p["resource_names"] = []any{} },
		"limit":                        func(c, p map[string]any) { c["kubernetes_permissions"] = make([]any, 33) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := accessConfig()
			p := c["kubernetes_permissions"].([]any)[0].(map[string]any)
			mutate(c, p)
			if _, err := kubernetesAccess(gen.Context{ComponentConfig: c}); err == nil {
				t.Fatal("unsafe Kubernetes access accepted")
			}
		})
	}
}

func TestGeneratedKubernetesIdentity(t *testing.T) {
	for _, target := range []string{"service", "worker", "rpc"} {
		t.Run(target, func(t *testing.T) {
			ctx := rpcContext()
			config := ctx.ComponentConfig
			file := "deploy/render/manifest.json.tmpl"
			switch target {
			case "worker":
				config = ctx.ComponentConfig["workers"].([]any)[0].(map[string]any)
				file = "deploy/render/worker-queue.json.tmpl"
			case "rpc":
				config = ctx.ComponentConfig["rpc_processes"].([]any)[0].(map[string]any)
				file = "deploy/render/rpc-records.json.tmpl"
			}
			for k, v := range accessConfig() {
				config[k] = v
			}
			p := permission()
			p["scope"] = "cluster"
			config["kubernetes_permissions"] = append(config["kubernetes_permissions"].([]any), p)
			files, _, err := (&Generator{}).Generate(ctx)
			if err != nil {
				t.Fatal(err)
			}
			checked := false
			for _, f := range files {
				if f.Path != file {
					continue
				}
				checked = true
				data := bytes.ReplaceAll(f.Content, []byte("{{.FSGroup}}"), []byte("10001"))
				data = bytes.ReplaceAll(data, []byte("{{.Namespace}}"), []byte("tenant-a"))
				var doc struct{ Items []map[string]any }
				if err := json.Unmarshal(data, &doc); err != nil {
					t.Fatal(err)
				}
				bindings := 0
				mounted := false
				for _, item := range doc.Items {
					kind := item["kind"].(string)
					if strings.HasSuffix(kind, "Binding") {
						bindings++
						subject := item["subjects"].([]any)[0].(map[string]any)
						if subject["namespace"] != "tenant-a" || subject["kind"] != "ServiceAccount" {
							t.Fatal("binding escaped namespace")
						}
						role := item["roleRef"].(map[string]any)
						if kind == "ClusterRoleBinding" && !strings.HasPrefix(role["name"].(string), "tenant-a.") {
							t.Fatal("cluster name can collide across installs")
						}
					}
					if kind != "Deployment" {
						continue
					}
					pod := item["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
					if pod["automountServiceAccountToken"] != false {
						t.Fatal("automatic token mount enabled")
					}
					for _, v := range pod["volumes"].([]any) {
						v := v.(map[string]any)
						if v["name"] != "kubernetes-api" {
							continue
						}
						projection := v["projected"].(map[string]any)
						if projection["defaultMode"] != float64(0440) {
							t.Fatal("token file permissions")
						}
						sources := projection["sources"].([]any)
						if len(sources) != 2 {
							t.Fatal("unexpected projected data")
						}
						token := sources[0].(map[string]any)["serviceAccountToken"].(map[string]any)
						if token["expirationSeconds"] != float64(3600) || token["path"] != "token" || token["audience"] != nil {
							t.Fatal("invalid API token projection")
						}
					}
					c := pod["containers"].([]any)[0].(map[string]any)
					for _, v := range c["volumeMounts"].([]any) {
						v := v.(map[string]any)
						if v["name"] == "kubernetes-api" {
							mounted = v["readOnly"] == true && v["mountPath"] == "/var/run/stego-kubernetes" && v["subPath"] == nil
						}
					}
				}
				if bindings != 2 || !mounted {
					t.Fatal("missing scoped bindings or rotating mount")
				}
			}
			if !checked {
				t.Fatal("target manifest missing")
			}
		})
	}
}

func TestKubernetesClusterNamesDoNotCollide(t *testing.T) {
	names := map[string]bool{}
	for _, pair := range [][2]string{{"team-one", "api"}, {"team", "one-api"}} {
		config := accessConfig()
		config["kubernetes_permissions"].([]any)[0].(map[string]any)["scope"] = "cluster"
		objects, err := kubernetesAccess(gen.Context{ServiceName: pair[1], ComponentConfig: config})
		if err != nil {
			t.Fatal(err)
		}
		name := strings.ReplaceAll(objects[0].(object)["metadata"].(object)["name"].(string), "{{.Namespace}}", pair[0])
		if names[name] {
			t.Fatal("separate installations share cluster RBAC")
		}
		names[name] = true
	}
}
