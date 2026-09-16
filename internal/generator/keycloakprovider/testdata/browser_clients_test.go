package keycloak

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

func browserInputs() (ClientBinding, BrowserClientPolicy) {
	return ClientBinding{ID: "browser-id", ClientID: "catalog-browser", Attributes: map[string]string{"stego.owner.product": "catalog"}}, BrowserClientPolicy{DisplayName: "Catalog", AccessTokenLifetimeSeconds: 300, RedirectURI: "https://catalog.example.test/auth/callback", PostLogoutRedirectURI: "https://catalog.example.test/auth/logged-out"}
}
func TestBrowserRedirectValidation(t *testing.T) {
	for _, uri := range []string{"https://catalog.example.test/auth/callback", "https://catalog.example.test:8443/auth/callback", "https://192.0.2.1/", "https://[2001:db8::1]:8443/oauth/return"} {
		if _, err := browserRedirectOrigin(uri); err != nil {
			t.Fatal("valid browser redirect rejected", uri, err)
		}
	}
	for _, uri := range []string{"http://catalog.example.test/auth/callback", "https://catalog.example.test", "https://catalog.example.test/*", "https://catalog.example.test/+", "https://catalog.example.test/a?b=c", "https://catalog.example.test/a#x", "https://catalog.example.test/a?", "https://catalog.example.test/a#", "https://user@catalog.example.test/a", "https://catalog.example.test/%2f", "https://catalog.example.test/../a", "https://catalog.example.test//a", "https://catalog.example.test/a\\b", "https://CATALOG.example.test/a", "https://catalog.example.test.:8443/a", "https://catalog.example.test:0/a", "https://catalog.example.test:65536/a", "https://catalog.example.test:08443/a", "https://catalog.example.test:/a", "https://127.1/a", "https://2130706433/a", "https://localhost/a", "https://app.localhost/a", "https://127.0.0.1/a", "https://[::1]/a", "https://[::ffff:192.0.2.1]/a"} {
		if _, err := browserRedirectOrigin(uri); err == nil {
			t.Fatal("unsafe browser redirect accepted", uri)
		}
	}
	calls := 0
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(500) })
	for _, change := range []func(*BrowserClientPolicy){func(p *BrowserClientPolicy) { p.RedirectURI = "" }, func(p *BrowserClientPolicy) { p.PostLogoutRedirectURI = "https://foreign.example.test/" }, func(p *BrowserClientPolicy) { p.AccessTokenLifetimeSeconds = 0 }} {
		b, p := browserInputs()
		change(&p)
		if _, err := c.CreateDisabledBrowserClient(context.Background(), b, p); err == nil {
			t.Fatal("invalid browser policy accepted")
		}
	}
	if calls != 0 {
		t.Fatal("invalid browser policy caused provider I/O")
	}
}
func TestBrowserProfileChecks(t *testing.T) {
	b, p := browserInputs()
	desired, err := browserClientConfiguration(b, p)
	if err != nil {
		t.Fatal(err)
	}
	if desired.PublicClient || !desired.StandardFlowEnabled || desired.Enabled || desired.Attributes["pkce.code.challenge.method"] != "S256" || desired.Attributes["post.logout.redirect.uris"] != p.PostLogoutRedirectURI {
		t.Fatal("unsafe browser profile")
	}
	for _, change := range []func(map[string]any){
		func(v map[string]any) { v["publicClient"] = true }, func(v map[string]any) { delete(v, "publicClient") },
		func(v map[string]any) { v["serviceAccountsEnabled"] = true }, func(v map[string]any) { v["directAccessGrantsEnabled"] = true },
		func(v map[string]any) { v["implicitFlowEnabled"] = true }, func(v map[string]any) { v["fullScopeAllowed"] = true },
		func(v map[string]any) { v["redirectUris"] = []string{p.RedirectURI + "*"} }, func(v map[string]any) { v["webOrigins"] = []string{"+"} },
		func(v map[string]any) { v["attributes"].(map[string]any)["post.logout.redirect.uris"] = "+" },
		func(v map[string]any) { v["attributes"].(map[string]any)["pkce.code.challenge.method"] = "plain" },
		func(v map[string]any) { v["attributes"].(map[string]any)["unknown.security.setting"] = "true" },
	} {
		raw, _ := json.Marshal(desired)
		var value map[string]any
		_ = json.Unmarshal(raw, &value)
		change(value)
		raw, _ = json.Marshal(value)
		var live ClientRepresentation
		_ = json.Unmarshal(raw, &live)
		if browserClientConfigurationMatches(live, raw, desired) {
			t.Fatal("unsafe browser drift accepted")
		}
	}
}
func newBrowserAccessFixture(t *testing.T) (*Client, *nativeAccessFixture, ClientBinding, BrowserAccessPolicy) {
	c, f, b, native := newNativeAccessFixture(t)
	_, base := browserInputs()
	desired, err := browserClientConfiguration(b, base)
	if err != nil {
		t.Fatal(err)
	}
	desired.Name = "old name"
	f.clients[b.ID] = desired
	return c, f, b, BrowserAccessPolicy{Client: base, Roles: native.Roles, Scopes: native.Scopes, Claims: native.Claims}
}
func TestBrowserAccessRepairAndFailure(t *testing.T) {
	for _, fault := range []string{"", "ignored enable", "post-enable drift", "post-enable read failure", "uncertain enable"} {
		t.Run(fault, func(t *testing.T) {
			c, f, b, p := newBrowserAccessFixture(t)
			f.fault = fault
			err := c.ReconcileBrowserClientAccess(context.Background(), b, p)
			if fault != "" {
				if err == nil || f.clients[b.ID].Enabled {
					t.Fatal("failed browser policy remained enabled", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			count := len(f.writes)
			if err = c.ReconcileBrowserClientAccess(context.Background(), b, p); err != nil || len(f.writes) != count {
				t.Fatal("correct browser policy caused writes", err)
			}
			desired := f.clients[b.ID]
			desired.PublicClient = true
			desired.Attributes["post.logout.redirect.uris"] = "+"
			f.clients[b.ID] = desired
			if err = c.InspectBrowserClientAccess(context.Background(), b, p); !errors.Is(err, ErrClientConfiguration) {
				t.Fatal("unsafe drift accepted", err)
			}
			if err = c.ReconcileBrowserClientAccess(context.Background(), b, p); err != nil {
				t.Fatal(err)
			}
			if err = c.InspectBrowserClientAccess(context.Background(), b, p); err != nil {
				t.Fatal(err)
			}
		})
	}
}
