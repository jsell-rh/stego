package browserapplication

import (
	"encoding/json"
	"github.com/jsell-rh/stego/internal/gen"
	"strings"
	"testing"
)

func fixture() (gen.Context, map[string]any, map[string]any) {
	app := map[string]any{"port": 8000, "image": "registry.example.test/app@sha256:" + strings.Repeat("a", 64), "listen_env": "LISTEN_ADDRESS", "port_env": "PORT", "env_secret": "app-env", "files_secret": "app-files", "health_path": "/api/v1/readyz"}
	deployment := map[string]any{"local_applications": []any{app}}
	ctx := gen.Context{ServiceName: "browser", PeerNamespaces: map[string]string{"browser-backend": "browser", "health-check": "health", "postgres-adapter": "store", "otel-tracing": "tracing", "kubernetes-service": "deploy"}, PeerConfigs: map[string]map[string]any{"browser-backend": {"asset_bundle": "ui/build.zip"}, "health-check": {"database": true}, "kubernetes-service": deployment}}
	return ctx, deployment, app
}
func TestPrivateApplicationContract(t *testing.T) {
	ctx, deployment, _ := fixture()
	before, _ := json.Marshal(ctx)
	c, err := Resolve(ctx, deployment)
	if err != nil || c.Port != 8000 {
		t.Fatal(c, err)
	}
	after, _ := json.Marshal(ctx)
	if string(before) != string(after) {
		t.Fatal("declaration was changed")
	}
	if c, err := Resolve(ctx, nil); err != nil || c != nil {
		t.Fatal("absent application changed the service")
	}
	cases := map[string]func(*gen.Context, map[string]any, map[string]any){
		"type":              func(_ *gen.Context, d, a map[string]any) { d["local_applications"] = a },
		"empty":             func(_ *gen.Context, d, a map[string]any) { d["local_applications"] = []any{} },
		"multiple":          func(_ *gen.Context, d, a map[string]any) { d["local_applications"] = []any{a, a} },
		"unknown":           func(_ *gen.Context, d, a map[string]any) { a["command"] = []any{"--public"} },
		"privileged port":   func(_ *gen.Context, d, a map[string]any) { a["port"] = 80 },
		"browser port":      func(_ *gen.Context, d, a map[string]any) { a["port"] = 8443 },
		"port type":         func(_ *gen.Context, d, a map[string]any) { a["port"] = "8000" },
		"mutable image":     func(_ *gen.Context, d, a map[string]any) { a["image"] = "registry.test/app:latest" },
		"implicit registry": func(_ *gen.Context, d, a map[string]any) { a["image"] = "team/app@sha256:" + strings.Repeat("a", 64) },
		"registry port": func(_ *gen.Context, d, a map[string]any) {
			a["image"] = "registry.test:99999/app@sha256:" + strings.Repeat("a", 64)
		},
		"environment alias":     func(_ *gen.Context, d, a map[string]any) { a["listen_env"] = "PORT" },
		"reserved environment":  func(_ *gen.Context, d, a map[string]any) { a["listen_env"] = "STEGO_BROWSER_ORIGIN" },
		"process environment":   func(_ *gen.Context, d, a map[string]any) { a["port_env"] = "LD_PRELOAD" },
		"browser files":         func(_ *gen.Context, d, a map[string]any) { a["files_secret"] = "browser-files" },
		"browser environment":   func(_ *gen.Context, d, a map[string]any) { a["env_secret"] = "browser-runtime" },
		"explicit secret alias": func(_ *gen.Context, d, a map[string]any) { d["env_secret"] = "app-files" },
		"health query":          func(_ *gen.Context, d, a map[string]any) { a["health_path"] = "/ready?token=value" },
		"health traversal":      func(_ *gen.Context, d, a map[string]any) { a["health_path"] = "/a/../ready" },
		"no browser":            func(c *gen.Context, d, a map[string]any) { delete(c.PeerNamespaces, "browser-backend") },
		"no deployment":         func(c *gen.Context, d, a map[string]any) { delete(c.PeerNamespaces, "kubernetes-service") },
		"bearer service":        func(c *gen.Context, d, a map[string]any) { c.PeerNamespaces["grpc-application"] = "rpc" },
		"no assets":             func(c *gen.Context, d, a map[string]any) { delete(c.PeerConfigs["browser-backend"], "asset_bundle") },
		"both assets":           func(c *gen.Context, d, a map[string]any) { c.PeerConfigs["browser-backend"]["assets"] = []any{} },
		"no database health":    func(c *gen.Context, d, a map[string]any) { c.PeerConfigs["health-check"]["database"] = false },
		"cluster identity":      func(c *gen.Context, d, a map[string]any) { d["kubernetes_api"] = true },
		"permissions":           func(c *gen.Context, d, a map[string]any) { d["kubernetes_permissions"] = []any{} },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			ctx, d, a := fixture()
			change(&ctx, d, a)
			if c, err := Resolve(ctx, d); err == nil || c != nil {
				t.Fatal("unsafe declaration accepted")
			}
		})
	}
}
