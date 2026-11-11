package oidcauth

import (
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
)

// context is the context of oidc-auth on the runtime data path with the settings given.
func context(config map[string]any) gen.Context {
	return gen.Context{RuntimeData: true, ComponentConfig: config}
}

func good() map[string]any {
	return map[string]any{"issuer": "https://sso.example.com/realms/shop", "audience": "orders-api",
		"algorithms": []any{"RS256", "ES256"}, "subject_claim": "sub", "header": "Authorization"}
}

func with(key string, value any) map[string]any {
	config := good()
	if value == nil {
		delete(config, key)
	} else {
		config[key] = value
	}
	return config
}

func TestGenerator(t *testing.T) {
	t.Run("ST-SUP-RC37-OC02-T02 A declaration that the runtime verifier would refuse is refused at generation", func(t *testing.T) {
		g := &Generator{}
		auth, err := g.RuntimeAuth(context(good()))
		if err != nil {
			t.Fatal(err)
		}
		if auth.Issuer != "https://sso.example.com/realms/shop" || auth.Audience != "orders-api" || strings.Join(auth.Algorithms, ",") != "RS256,ES256" ||
			auth.SubjectClaim != "sub" || auth.TenantClaim != "" {
			t.Fatalf("the settings are %+v", auth)
		}
		if auth, err := g.RuntimeAuth(context(with("algorithms", nil))); err != nil || strings.Join(auth.Algorithms, ",") != "RS256" {
			t.Fatalf("no algorithms gave %+v, %v, want RS256 alone", auth, err)
		}
		files, wiring, err := g.Generate(context(good()))
		if err != nil || len(files) != 0 || wiring == nil || len(wiring.Constructors) != 0 || len(wiring.Imports) != 0 {
			t.Fatalf("Generate gave %d files and %+v, %v, want no file and no wiring (ST-SUP-RC37-OC04)", len(files), wiring, err)
		}
		for name, config := range map[string]map[string]any{
			"an http issuer":             with("issuer", "http://sso.example.com/realms/shop"),
			"an issuer with a query":     with("issuer", "https://sso.example.com/realms/shop?x=1"),
			"an issuer with a user":      with("issuer", "https://u@sso.example.com/realms/shop"),
			"an issuer with a fragment":  with("issuer", "https://sso.example.com/realms/shop#a"),
			"no issuer":                  with("issuer", nil),
			"no audience":                with("audience", nil),
			"an audience with a space":   with("audience", " orders-api"),
			"alg none":                   with("algorithms", []any{"none"}),
			"HS256":                      with("algorithms", []any{"HS256"}),
			"PS256":                      with("algorithms", []any{"RS256", "PS256"}),
			"RS256 twice":                with("algorithms", []any{"RS256", "RS256"}),
			"algorithms not a list":      with("algorithms", "RS256"),
			"an algorithm not text":      with("algorithms", []any{256}),
			"the subject claim exp":      with("subject_claim", "exp"),
			"a subject claim path":       with("subject_claim", "a.b"),
			"a role claim with a space":  with("roles_claim", "realm access"),
			"a header with a colon":      with("header", "X-Token:"),
			"a relative CA file":         with("ca_file", "certs/ca.pem"),
			"an issuer that is not text": with("issuer", 7),
		} {
			t.Run(name, func(t *testing.T) {
				if err := g.ValidateContext(context(config)); err == nil || !strings.Contains(err.Error(), Component) {
					t.Fatalf("the declaration with %s gave %v, want a refusal that names %s", name, err, Component)
				}
			})
		}
		store := context(good())
		store.RuntimeData = false
		if err := g.ValidateContext(store); err == nil || !strings.Contains(err.Error(), "runtime data path") {
			t.Fatalf("oidc-auth on the GORM store gave %v, want a refusal", err)
		}
	})
}
