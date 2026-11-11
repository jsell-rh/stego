package rhssoauth

import (
	"go/format"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
)

func TestGenerateSharedRuntime(t *testing.T) {
	for _, ns := range []string{"", "internal/auth", "pkg/authn"} {
		ctx := gen.Context{OutputNamespace: ns, ServiceName: "test-api", BasePath: "/api/v1", ErrorTypeBase: "https://errors.example/", ComponentConfig: map[string]any{"issuer": "https://issuer.example", "audience": "test", "jwk_cert_file": "/etc/jwks.json", "public_paths": []any{"/health", "/api/v1/ready"}}}
		files, wiring, err := new(Generator).Generate(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(files) != 7 {
			t.Fatalf("got %d files", len(files))
		}
		if !wiring.ConstructorReturnsError[0] || wiring.ConstructorDeferCalls[0] != "Stop()" || wiring.MiddlewareWrapExpr != "%s.Build()(%s)" {
			t.Fatal("startup errors or shutdown are not wired")
		}
		if resources := wiring.ConstructorResources[0]; len(resources) != 1 || resources[0] != gen.ServiceContext {
			t.Fatal("SSO startup does not use the service context")
		}
		if len(wiring.GoModRequires) != 1 || wiring.GoModRequires["github.com/golang-jwt/jwt/v5"] == "" {
			t.Fatal("SSO does not use the common JWT dependency")
		}
		source := map[string]string{}
		for _, f := range files {
			if _, err := format.Source(f.Bytes()); err != nil {
				t.Fatal(err)
			}
			source[f.Path] = string(f.Content)
		}
		prefix := ns
		if prefix == "" {
			prefix = "internal/auth"
		}
		for _, text := range []string{"/api/v1/health", "/api/v1/ready", "/api/v1/openapi", "https://issuer.example", "/etc/jwks.json"} {
			if !strings.Contains(source[prefix+"/sso.go"], text) {
				t.Fatalf("missing SSO configuration %s", text)
			}
		}
		if !strings.Contains(source[prefix+"/middleware.go"], "TEST-AUT-001") {
			t.Fatal("SSO error code changed")
		}
	}
}

func TestSSOConfigurationValidation(t *testing.T) {
	for _, config := range []map[string]any{
		{"issuer": 42}, {"issuer": "http://issuer.example"}, {"audience": " test"},
		{"jwk_cert_url": "https://user:secret@example.com/keys"}, {"jwk_cert_file": false},
		{"public_paths": "/private"}, {"public_paths": []any{42}}, {"public_paths": []any{"/private/../admin"}},
		{"public_paths": []any{"private"}}, {"public_paths": []any{"/private?skip=true"}},
	} {
		if _, _, err := new(Generator).Generate(gen.Context{ComponentConfig: config}); err == nil {
			t.Fatalf("accepted invalid config: %#v", config)
		}
	}
	paths, err := publicPaths(gen.Context{ComponentConfig: map[string]any{"public_paths": []any{}}})
	if err != nil || len(paths) != 1 || paths[0] != "/openapi" {
		t.Fatal("an empty path list must not restore defaults")
	}
	paths, err = publicPaths(gen.Context{BasePath: "/api", ComponentConfig: map[string]any{"public_paths": []any{"/api2/health"}}})
	if err != nil || paths[0] != "/api/api2/health" {
		t.Fatal("base path requires a segment boundary")
	}
}

func TestRuntimeAuth(t *testing.T) {
	t.Run("ST-SUP-RC21-OC15-T02 rh-sso-auth on the runtime data path takes the Red Hat SSO form and claims only", func(t *testing.T) {
		config := func(change map[string]any) gen.Context {
			c := map[string]any{"issuer": "https://sso.redhat.com/auth/realms/redhat-external", "audience": "orders-api",
				"jwk_cert_url": defaultKeysURL, "public_paths": []any{"/healthcheck", "/metrics"}}
			for k, v := range change {
				if v == nil {
					delete(c, k)
				} else {
					c[k] = v
				}
			}
			return gen.Context{RuntimeData: true, ComponentConfig: c}
		}
		g := &Generator{}
		auth, err := g.RuntimeAuth(config(map[string]any{"jwk_ca_file": "/etc/sso/ca.pem"}))
		if err != nil {
			t.Fatal(err)
		}
		if auth.Issuer != "https://sso.redhat.com/auth/realms/redhat-external" || auth.SubjectClaim != "sub" || auth.TenantClaim != "org_id" ||
			len(auth.Algorithms) != 1 || auth.Algorithms[0] != "RS256" || auth.CAFile != "/etc/sso/ca.pem" || auth.Audience != "orders-api" {
			t.Fatalf("the settings are %+v", auth)
		}
		for name, change := range map[string]map[string]any{
			"an issuer of another form": {"issuer": "https://sso.redhat.com/realms/redhat-external"},
			"an issuer with a subpath":  {"issuer": "https://sso.redhat.com/auth/realms/redhat-external/x"},
			"an http issuer":            {"issuer": "http://sso.redhat.com/auth/realms/redhat-external"},
			"a key file":                {"jwk_cert_file": "/etc/sso/certs.json"},
			"another key URL":           {"jwk_cert_url": "https://keys.example.com/certs"},
			"other public paths":        {"public_paths": []any{"/healthcheck"}},
			"no audience":               {"audience": nil},
		} {
			t.Run(name, func(t *testing.T) {
				if _, err := g.RuntimeAuth(config(change)); err == nil || !strings.Contains(err.Error(), "rh-sso-auth") {
					t.Fatalf("rh-sso-auth with %s gave %v, want a refusal that names rh-sso-auth", name, err)
				}
			})
		}
	})
}
