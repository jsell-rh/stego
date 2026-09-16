package keycloak

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func testLiveBrowserClient(t *testing.T, c *Client, ctx context.Context, caFile string) {
	t.Helper()
	b, base := browserInputs()
	created, err := c.CreateDisabledBrowserClient(ctx, b, base)
	if err != nil {
		t.Fatal("real browser creation failed", err)
	}
	if !validSecretCreationTime(created.Attributes[clientSecretCreationTime]) {
		t.Fatal("browser creation lost provider secret metadata")
	}
	if _, err := c.CreateDisabledBrowserClient(ctx, b, base); !errors.Is(err, ErrConflict) {
		t.Fatal("browser conflict differs", err)
	}
	p := BrowserAccessPolicy{Client: base, Roles: []string{"open"}, Scopes: RolePolicy{Clients: []ClientRoleGrant{{Client: b, Names: []string{"open"}}}}, Claims: TokenClaimsPolicy{AudienceClients: []ClientBinding{b}, ClientRoles: []ClientRoleClaim{{Client: b, Claim: "catalog.roles"}}}}
	if err := c.ReconcileBrowserClientAccess(ctx, b, p); err != nil {
		t.Fatal("real browser access failed", err)
	}
	if err := c.ReconcileUserClientRoles(ctx, b, nativeTestSubject, []string{"open"}); err != nil {
		t.Fatal(err)
	}
	secret, err := c.GetClientSecret(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	browser := newNativeBrowser(t, c, ctx, caFile)
	authPath := "/realms/provider-test/protocol/openid-connect/auth"
	tokenPath := "/realms/provider-test/protocol/openid-connect/token"
	for _, bad := range []string{"missing-pkce", "foreign-redirect", "wildcard-path"} {
		query := url.Values{"client_id": {b.ClientID}, "response_type": {"code"}, "scope": {"openid"}, "redirect_uri": {base.RedirectURI}, "state": {"browser-test"}}
		if bad != "missing-pkce" {
			query.Set("code_challenge", strings.Repeat("a", 43))
			query.Set("code_challenge_method", "S256")
		}
		if bad == "foreign-redirect" {
			query.Set("redirect_uri", "https://foreign.example.test/auth/callback")
		}
		if bad == "wildcard-path" {
			query.Set("redirect_uri", base.RedirectURI+"/other")
		}
		status, headers, _ := browser(http.MethodGet, authPath+"?"+query.Encode(), nil)
		if status == 400 && headers.Get("Location") == "" {
			continue
		}
		target, e := url.Parse(headers.Get("Location"))
		if bad != "missing-pkce" || e != nil || (status != 302 && status != 303) || target.Query().Get("error") != "invalid_request" || target.Query().Get("code") != "" {
			t.Fatal("unsafe browser authorization accepted", bad, status)
		}
	}
	var idToken string
	for _, mode := range []string{"missing secret", "wrong verifier", "valid"} {
		code, verifier, nonce := nativeAuthorizationCode(t, browser, authPath, b.ClientID, base.RedirectURI)
		form := url.Values{"grant_type": {"authorization_code"}, "client_id": {b.ClientID}, "redirect_uri": {base.RedirectURI}, "code": {code}, "code_verifier": {verifier}}
		if mode != "missing secret" {
			form.Set("client_secret", secret.Reveal())
		}
		if mode == "wrong verifier" {
			form.Set("code_verifier", strings.Repeat("z", 43))
		}
		status, _, body := browser(http.MethodPost, tokenPath, form)
		var grant struct {
			Access string `json:"access_token"`
			ID     string `json:"id_token"`
			Error  string `json:"error"`
		}
		if decode(body, &grant) != nil {
			t.Fatal("invalid browser token response")
		}
		if mode != "valid" {
			if status < 400 || grant.Access != "" || grant.ID != "" {
				t.Fatal("unsafe browser code exchange accepted", mode, status)
			}
			continue
		}
		if status != 200 || grant.Access == "" || grant.ID == "" {
			t.Fatal("browser code exchange failed", status)
		}
		access := verifyLiveMapperToken(t, c, ctx, grant.Access)
		identity := verifyLiveMapperToken(t, c, ctx, grant.ID)
		if !nativeAudience(access["aud"], b.ClientID) || access["sub"] != nativeTestSubject || identity["nonce"] != nonce || !nativeAudience(identity["aud"], b.ClientID) {
			t.Fatal("browser signed identity differs")
		}
		idToken = grant.ID
		status, _, _ = browser(http.MethodPost, tokenPath, form)
		if status < 400 {
			t.Fatal("browser code was reused")
		}
	}
	logout := url.Values{"id_token_hint": {idToken}, "post_logout_redirect_uri": {"https://foreign.example.test/"}}
	status, _, _ := browser(http.MethodGet, "/realms/provider-test/protocol/openid-connect/logout?"+logout.Encode(), nil)
	if status != 400 {
		t.Fatal("foreign logout accepted", status)
	}
	logout.Set("post_logout_redirect_uri", base.PostLogoutRedirectURI)
	status, headers, _ := browser(http.MethodGet, "/realms/provider-test/protocol/openid-connect/logout?"+logout.Encode(), nil)
	if (status != 302 && status != 303) || headers.Get("Location") != base.PostLogoutRedirectURI {
		t.Fatal("browser logout target differs", status)
	}
	drift, _ := json.Marshal(map[string]any{"publicClient": true, "attributes": map[string]string{"post.logout.redirect.uris": "+"}})
	response, err := c.admin(ctx, http.MethodPut, "/clients/"+b.ID, drift)
	if err != nil || response.StatusCode != 204 {
		t.Fatal("browser drift fixture failed", err)
	}
	if err = c.ReconcileBrowserClientAccess(ctx, b, p); err != nil {
		desired, e := browserClientConfiguration(b, base)
		if e != nil {
			t.Fatal(e)
		}
		reportClientDifference(t, c, ctx, b, desired)
		t.Fatal("real browser repair failed", err)
	}
	if err = c.InspectBrowserClientAccess(ctx, b, p); err != nil {
		t.Fatal(err)
	}
	repaired, err := c.InspectClient(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	if !validSecretCreationTime(repaired.Attributes[clientSecretCreationTime]) {
		t.Fatal("browser repair lost provider secret metadata")
	}
	if err = c.DisableClient(ctx, b); err != nil {
		t.Fatal(err)
	}
	if err = c.DeleteClient(ctx, b); err != nil {
		t.Fatal(err)
	}
	t.Log("Real browser client: confidential code exchange, PKCE, exact callbacks, signed audience, one-use code, logout, drift repair, and cleanup passed")
}
