package kubernetesservice

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"text/template"
)

func TestPrivateApplicationDeployment(t *testing.T) {
	ctx := serviceContext()
	delete(ctx.PeerNamespaces, "rest-api")
	for key, value := range map[string]string{"browser-backend": "browser", "health-check": "health", "postgres-adapter": "store", "otel-tracing": "tracing", "kubernetes-service": "deploy"} {
		ctx.PeerNamespaces[key] = value
	}
	ctx.ComponentConfig["local_applications"] = []any{map[string]any{"port": 8000, "image": "registry.example.test/app@sha256:" + strings.Repeat("a", 64), "listen_env": "LISTEN_ADDRESS", "port_env": "PORT", "env_secret": "app-env", "files_secret": "app-files", "health_path": "/api/v1/readyz"}}
	ctx.PeerConfigs = map[string]map[string]any{"browser-backend": {"asset_bundle": "ui/build.zip"}, "health-check": {"database": true}, "kubernetes-service": ctx.ComponentConfig}
	files, _, err := new(Generator).Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	repeat, _, err := new(Generator).Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var document struct{ Items []object }
	for i, file := range files {
		if file.Path != repeat[i].Path || !bytes.Equal(file.Bytes(), repeat[i].Bytes()) {
			t.Fatal("deployment changed across generation")
		}
		if !strings.HasSuffix(file.Path, "/manifest.json.tmpl") {
			continue
		}
		parsed, err := template.New("deployment").Parse(string(file.Content))
		if err != nil {
			t.Fatal(err)
		}
		var output bytes.Buffer
		if err := parsed.Execute(&output, struct {
			Image, Namespace string
			FSGroup          uint64
		}{"registry.test/browser@sha256:" + strings.Repeat("b", 64), "gateway-one", 10001}); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(output.Bytes(), &document); err != nil {
			t.Fatal(err)
		}
	}
	var pod object
	for _, item := range document.Items {
		spec, _ := item["spec"].(map[string]any)
		switch item["kind"] {
		case "Deployment":
			pod = spec["template"].(map[string]any)["spec"].(map[string]any)
		case "Service":
			ports := spec["ports"].([]any)
			if len(ports) != 1 || ports[0].(map[string]any)["port"] != float64(8443) {
				t.Fatal("application has a service path")
			}
		case "NetworkPolicy":
			for _, raw := range spec["ingress"].([]any) {
				for _, port := range raw.(map[string]any)["ports"].([]any) {
					if port.(map[string]any)["port"] != float64(8443) {
						t.Fatal("application ingress bypasses browser")
					}
				}
			}
		}
	}
	if pod == nil || pod["automountServiceAccountToken"] != false || pod["hostNetwork"] != nil || pod["shareProcessNamespace"] != nil {
		t.Fatal("private Pod boundary is missing")
	}
	containers := pod["containers"].([]any)
	if len(containers) != 2 {
		t.Fatal("expected one browser and one application")
	}
	browser, app := containers[0].(map[string]any), containers[1].(map[string]any)
	if app["ports"] != nil {
		t.Fatal("application declares a public port")
	}
	for _, container := range []map[string]any{browser, app} {
		security := container["securityContext"].(map[string]any)
		if security["allowPrivilegeEscalation"] != false || security["readOnlyRootFilesystem"] != true || security["capabilities"].(map[string]any)["drop"].([]any)[0] != "ALL" {
			t.Fatal("container security changed")
		}
		if container["resources"] == nil {
			t.Fatal("container limits missing")
		}
	}
	env := map[string]string{}
	for _, raw := range app["env"].([]any) {
		entry := raw.(map[string]any)
		env[entry["name"].(string)] = entry["value"].(string)
	}
	if env["LISTEN_ADDRESS"] != "127.0.0.1" || env["PORT"] != "8000" {
		t.Fatal("local listener is not fixed")
	}
	appMounts := map[string]bool{}
	for _, raw := range app["volumeMounts"].([]any) {
		appMounts[raw.(map[string]any)["name"].(string)] = true
	}
	for _, raw := range browser["volumeMounts"].([]any) {
		if appMounts[raw.(map[string]any)["name"].(string)] {
			t.Fatal("browser and application share a volume")
		}
	}
	if len(appMounts) != 2 || !appMounts["application-files"] || !appMounts["application-tmp"] {
		t.Fatal("application mounts differ")
	}
	ctx.ComponentConfig["network_peers"].([]any)[0].(map[string]any)["port"] = 8000
	if _, _, err := new(Generator).Generate(ctx); err == nil {
		t.Fatal("application port can bypass the browser")
	}
}
