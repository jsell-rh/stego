package keycloak

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

func testLiveNativeClients(t *testing.T, c *Client, ctx context.Context) {
	t.Helper()
	for _, item := range []struct {
		name, redirect, callback, role, claim string
		device                                bool
	}{
		{"catalog-native", "http://127.0.0.1:*", "http://127.0.0.1:38123", "catalog-open", "catalog.roles", true},
		{"pipeline-native", "http://[::1]:*/return", "http://[::1]:38124/return", "pipeline-run", "pipeline.permissions", false},
	} {
		b := ClientBinding{ID: item.name, ClientID: item.name, Attributes: map[string]string{"stego.owner.native-test": item.name}}
		p := NativeClientPolicy{DisplayName: item.name, AccessTokenLifetimeSeconds: 300, LoopbackRedirectURIs: []string{item.redirect}, EnableDeviceAuthorization: item.device}
		if _, err := c.CreateDisabledNativeClient(ctx, b, p); err != nil {
			reportNativeDifference(t, c, ctx, b, p)
			t.Fatal("real native creation failed", err)
		}
		if _, err := c.CreateDisabledNativeClient(ctx, b, p); !errors.Is(err, ErrConflict) {
			t.Fatal("real native creation conflict differs", err)
		}
		if _, err := c.EnsureClientRoles(ctx, b, []string{item.role}); err != nil {
			t.Fatal(err)
		}
		if err := c.ReconcileUserClientRoles(ctx, b, "native-user", []string{item.role}); err != nil {
			t.Fatal(err)
		}
		scopes := RolePolicy{Clients: []ClientRoleGrant{{Client: b, Names: []string{item.role}}}}
		claims := TokenClaimsPolicy{AudienceClients: []ClientBinding{b}, ClientRoles: []ClientRoleClaim{{Client: b, Claim: item.claim}}}
		if err := c.ReconcileClientScopes(ctx, b, scopes); err != nil {
			t.Fatal(err)
		}
		if err := c.ReconcileTokenMappers(ctx, b, claims); err != nil {
			t.Fatal(err)
		}
		// A changed callback and display name must be repaired while disabled.
		drift, _ := json.Marshal(map[string]any{"name": "drift", "redirectUris": []string{"https://foreign.invalid/callback"}})
		response, err := c.admin(ctx, http.MethodPut, "/clients/"+b.ID, drift)
		if err != nil || response.StatusCode != 204 {
			t.Fatal("native drift fixture failed", err)
		}
		if _, err = c.InspectDisabledNativeClient(ctx, b, p); !errors.Is(err, ErrClientConfiguration) {
			t.Fatal("native drift accepted", err)
		}
		for i := 0; i < 2; i++ {
			if err = c.ConfigureDisabledNativeClient(ctx, b, p); err != nil {
				t.Fatal("real native repair failed", err)
			}
		}
		if err = c.InspectClientScopes(ctx, b, scopes); err != nil {
			t.Fatal("native repair changed scopes", err)
		}
		if err = c.InspectTokenMappers(ctx, b, claims); err != nil {
			t.Fatal("native repair changed mappers", err)
		}
		// Only this fixture enables login. Production enablement is a separate gate.
		response, err = c.admin(ctx, http.MethodPut, "/clients/"+b.ID, []byte(`{"enabled":true}`))
		if err != nil || response.StatusCode != 204 {
			t.Fatal("enable native fixture", err)
		}
		browser := newNativeBrowser(t, c, ctx)
		authPath := "/realms/provider-test/protocol/openid-connect/auth"
		for _, bad := range []string{"missing-pkce", "foreign-redirect"} {
			query := url.Values{"client_id": {b.ClientID}, "response_type": {"code"}, "scope": {"openid"}, "redirect_uri": {item.callback}, "state": {"invalid-request-test"}}
			if bad == "foreign-redirect" {
				query.Set("redirect_uri", "https://foreign.invalid/callback")
				query.Set("code_challenge", strings.Repeat("a", 43))
				query.Set("code_challenge_method", "S256")
			}
			status, headers, _ := browser(http.MethodGet, authPath+"?"+query.Encode(), nil)
			if status == http.StatusBadRequest && headers.Get("Location") == "" {
				continue
			}
			location, e := url.Parse(headers.Get("Location"))
			if bad == "foreign-redirect" || e != nil || (status != 302 && status != 303) || location.Query().Get("error") != "invalid_request" || location.Query().Get("code") != "" {
				t.Fatal("unsafe native authorization request accepted", bad, status)
			}
		}
		tokenPath := "/realms/provider-test/protocol/openid-connect/token"
		// A wrong verifier must fail. Use a fresh authorization code for success.
		for _, wrong := range []bool{true, false} {
			code, verifier, nonce := nativeAuthorizationCode(t, browser, authPath, b.ClientID, item.callback)
			if wrong {
				verifier = strings.Repeat("z", 43)
			}
			form := url.Values{"grant_type": {"authorization_code"}, "client_id": {b.ClientID}, "redirect_uri": {item.callback}, "code": {code}, "code_verifier": {verifier}}
			status, _, body := browser(http.MethodPost, tokenPath, form)
			var grant struct {
				Access string `json:"access_token"`
				ID     string `json:"id_token"`
				Error  string `json:"error"`
				Type   string `json:"token_type"`
			}
			if decode(body, &grant) != nil {
				t.Fatal("invalid native grant response")
			}
			if wrong {
				if status != 400 || grant.Error != "invalid_grant" || grant.Access != "" {
					t.Fatal("wrong PKCE verifier accepted")
				}
				continue
			}
			if status != 200 || !strings.EqualFold(grant.Type, "Bearer") || grant.Access == "" || grant.ID == "" {
				t.Fatal("native authorization-code exchange failed", status)
			}
			access := verifyLiveMapperToken(t, c, ctx, grant.Access)
			id := verifyLiveMapperToken(t, c, ctx, grant.ID)
			if access["iss"] != c.Issuer() || access["sub"] != "native-user" || access["azp"] != b.ClientID || !nativeAudience(access["aud"], b.ClientID) || id["iss"] != c.Issuer() || !nativeAudience(id["aud"], b.ClientID) || id["sub"] != "native-user" || id["nonce"] != nonce {
				t.Fatal("native signed identity differs")
			}
			pieces := strings.Split(item.claim, ".")
			object, ok := access[pieces[0]].(map[string]any)
			if !ok {
				t.Fatal("native role claim is absent")
			}
			roles, ok := object[pieces[1]].([]any)
			if !ok || len(roles) != 1 || roles[0] != item.role {
				t.Fatal("native role claim differs")
			}
			if _, ok = id[pieces[0]]; ok {
				t.Fatal("access-only role claim entered ID token")
			}
			status, _, body = browser(http.MethodPost, tokenPath, form)
			grant = struct {
				Access string `json:"access_token"`
				ID     string `json:"id_token"`
				Error  string `json:"error"`
				Type   string `json:"token_type"`
			}{}
			if decode(body, &grant) != nil || status != 400 || grant.Error != "invalid_grant" || grant.Access != "" {
				t.Fatal("native authorization code was reusable")
			}
		}
		status, _, body := browser(http.MethodPost, "/realms/provider-test/protocol/openid-connect/auth/device", url.Values{"client_id": {b.ClientID}})
		var device struct {
			Code     string `json:"device_code"`
			UserCode string `json:"user_code"`
			Error    string `json:"error"`
		}
		if decode(body, &device) != nil {
			t.Fatal("invalid device grant response")
		}
		if item.device {
			if status != 200 || device.Code == "" || device.UserCode == "" {
				t.Fatal("configured native device flow failed", status)
			}
		} else if status != 400 || device.Code != "" || device.Error == "" {
			t.Fatal("disabled native device flow accepted", status)
		}
		if err = c.DisableClient(ctx, b); err != nil {
			t.Fatal(err)
		}
		if _, err = c.InspectDisabledNativeClient(ctx, b, p); err != nil {
			t.Fatal("native profile changed after login", err)
		}
		if err = c.DeleteClient(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	t.Log("Real native checks: two policies; disabled creation and repair; PKCE S256 login; wrong verifier, foreign callback, and code replay denied; signed identity and role claims; device policy; deletion")
}

type nativeBrowser func(string, string, url.Values) (int, http.Header, []byte)

// This test drives protocol requests, not a browser UI. Cookies remain in this
// fixture. Every request stays at the verified issuer; redirects are not followed.
func newNativeBrowser(t *testing.T, c *Client, ctx context.Context) nativeBrowser {
	t.Helper()
	issuer, err := url.Parse(c.Issuer())
	if err != nil {
		t.Fatal(err)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return func(method, path string, form url.Values) (int, http.Header, []byte) {
		t.Helper()
		reference, e := url.Parse(path)
		if e != nil {
			t.Fatal("invalid native fixture path")
		}
		u := issuer.ResolveReference(reference)
		if len(u.String()) > 16384 || u.Scheme != issuer.Scheme || u.Host != issuer.Host || u.User != nil || u.Fragment != "" {
			t.Fatal("native fixture request escaped issuer")
		}
		headers := http.Header{}
		request := http.Request{Header: headers}
		for _, cookie := range jar.Cookies(u) {
			request.AddCookie(cookie)
		}
		var body []byte
		if form != nil {
			body = []byte(form.Encode())
			headers.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		result, e := c.http.Do(ctx, method, u.RequestURI(), headers, body)
		if e != nil {
			t.Fatal("native fixture request failed", e)
		}
		if len(result.Body) > 1<<20 {
			t.Fatal("native fixture response is too large")
		}
		response := http.Response{Header: result.Header}
		cookies := response.Cookies()
		if len(cookies) > 32 {
			t.Fatal("too many native fixture cookies")
		}
		jar.SetCookies(u, cookies)
		return result.StatusCode, result.Header, result.Body
	}
}
func nativeAuthorizationCode(t *testing.T, browser nativeBrowser, path, clientID, redirect string) (string, string, string) {
	t.Helper()
	random := func() string {
		t.Helper()
		value := make([]byte, 32)
		if _, err := rand.Read(value); err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(value)
	}
	verifier, state, nonce := random(), random(), random()
	sum := sha256.Sum256([]byte(verifier))
	query := url.Values{"client_id": {clientID}, "response_type": {"code"}, "scope": {"openid"}, "redirect_uri": {redirect}, "state": {state}, "nonce": {nonce}, "prompt": {"login"}, "code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}}
	status, headers, body := browser(http.MethodGet, path+"?"+query.Encode(), nil)
	for step := 0; step < 8; step++ {
		if status == 200 {
			root, err := html.Parse(strings.NewReader(string(body)))
			if err != nil {
				t.Fatal("invalid native login form")
			}
			action := ""
			var visit func(*html.Node)
			visit = func(n *html.Node) {
				if n.Type == html.ElementNode && n.Data == "form" {
					id, value := "", ""
					for _, a := range n.Attr {
						if a.Key == "id" {
							id = a.Val
						}
						if a.Key == "action" {
							value = a.Val
						}
					}
					if id == "kc-form-login" {
						if action != "" {
							t.Fatal("ambiguous native login form")
						}
						action = value
					}
				}
				for child := n.FirstChild; child != nil; child = child.NextSibling {
					visit(child)
				}
			}
			visit(root)
			if action == "" {
				t.Fatal("native login form is missing")
			}
			status, headers, body = browser(http.MethodPost, action, url.Values{"username": {"native-user"}, "password": {"provider-test-native-password"}, "credentialId": {""}})
			continue
		}
		if status != 302 && status != 303 {
			t.Fatal("native authorization failed", status)
		}
		target, err := url.Parse(headers.Get("Location"))
		if err != nil {
			t.Fatal("invalid native authorization redirect")
		}
		if target.Scheme == "http" {
			base := *target
			base.RawQuery = ""
			base.ForceQuery = false
			if base.String() != redirect || target.Query().Get("state") != state || target.Query().Get("code") == "" || target.Query().Get("error") != "" {
				t.Fatal("native callback differs")
			}
			return target.Query().Get("code"), verifier, nonce
		}
		status, headers, body = browser(http.MethodGet, target.String(), nil)
	}
	t.Fatal("native authorization exceeded redirect limit")
	return "", "", ""
}

func nativeAudience(value any, want string) bool {
	switch audience := value.(type) {
	case string:
		return audience == want
	case []any:
		return len(audience) == 1 && audience[0] == want
	}
	return false
}

// Report names only. Configuration values and provider bodies stay private.
func reportNativeDifference(t *testing.T, c *Client, ctx context.Context, b ClientBinding, p NativeClientPolicy) {
	t.Helper()
	value, _, err := c.boundClient(ctx, b)
	if err != nil {
		t.Log("native difference read failed", err)
		return
	}
	desired, err := nativeClientConfiguration(b, p)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(value)
	d, _ := json.Marshal(desired)
	var actual, wanted map[string]json.RawMessage
	_ = json.Unmarshal(a, &actual)
	_ = json.Unmarshal(d, &wanted)
	for key, v := range wanted {
		if key != "attributes" && !bytes.Equal(v, actual[key]) {
			t.Log("native field differs:", key)
		}
	}
	for key, v := range desired.Attributes {
		if value.Attributes[key] != v {
			t.Log("native attribute differs:", key)
		}
	}
	for key := range value.Attributes {
		if _, ok := desired.Attributes[key]; !ok {
			t.Log("extra native attribute:", key)
		}
	}
}
