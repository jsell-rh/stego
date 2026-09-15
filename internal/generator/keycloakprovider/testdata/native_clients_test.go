package keycloak

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"testing"
)

func nativeInputs() (ClientBinding, NativeClientPolicy) {
	return ClientBinding{ID: "native-id", ClientID: "catalog-cli", Attributes: map[string]string{"stego.owner.product": "catalog"}}, NativeClientPolicy{DisplayName: "Catalog CLI", AccessTokenLifetimeSeconds: 300, LoopbackRedirectURIs: []string{"http://127.0.0.1:*", "http://localhost:*"}, EnableDeviceAuthorization: true}
}
func nativeRecord() map[string]any {
	return map[string]any{
		"frontchannelLogout": false, "surrogateAuthRequired": false, "authenticationFlowBindingOverrides": map[string]string{},
		"id": "native-id", "clientId": "catalog-cli", "name": "Catalog CLI", "protocol": "openid-connect", "clientAuthenticatorType": "client-secret",
		"enabled": false, "publicClient": true, "bearerOnly": false, "consentRequired": false, "serviceAccountsEnabled": false, "standardFlowEnabled": true, "implicitFlowEnabled": false, "directAccessGrantsEnabled": false, "fullScopeAllowed": false,
		"redirectUris": []string{"http://127.0.0.1", "http://localhost"}, "webOrigins": []string{}, "defaultClientScopes": []string{}, "optionalClientScopes": []string{},
		"attributes": map[string]string{"stego.owner.product": "catalog", "access.token.lifespan": "300", "client_credentials.use_refresh_token": "false", "oauth2.device.authorization.grant.enabled": "true", "oidc.ciba.grant.enabled": "false", "standard.token.exchange.enabled": "false", "oauth2.jwt.authorization.grant.enabled": "false", "external.token.enabled": "false", "pkce.code.challenge.method": "S256", "backchannel.logout.session.required": "true", "backchannel.logout.revoke.offline.tokens": "true", "realm_client": "false"},
	}
}

func TestNativeClientRedirectValidation(t *testing.T) {
	for _, uri := range []string{"http://127.0.0.1:*", "http://localhost:*", "http://[::1]:*/callback", "http://127.0.0.2:34567/oauth/callback", "http://[::1]:8000/"} {
		if !validLoopbackRedirect(uri) {
			t.Fatal("valid loopback redirect rejected", uri)
		}
	}
	for _, uri := range []string{"http://127.0.0.2:*/callback", "http://::1:8000/", "http://localhost:8000/%2a", "http://localhost:8000/%252f", "https://app.example/callback", "http://example.com:8000/callback", "http://127.0.0.1.evil:8000/", "http://0.0.0.0:8000/", "http://127.1:8000/", "http://2130706433:8000/", "http://[::]:8000/", "http://localhost:0/", "http://localhost:65536/", "http://localhost:08000/", "http://localhost/", "http://localhost:*/other*", "http://localhost:8000/?next=elsewhere", "http://localhost:8000/?", "http://localhost:8000/#", "http://user@localhost:8000/", "http://localhost:8000/../elsewhere", "http://localhost:8000/%2e%2e/elsewhere", "http://localhost:8000/a%2fb", "http://localhost:8000/%00", "http://localhost:8000/a\\b", "http://[::1%25zone]:8000/"} {
		if validLoopbackRedirect(uri) {
			t.Fatal("unsafe redirect accepted", uri)
		}
	}
	calls := 0
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(500) })
	for _, uris := range [][]string{nil, {"http://localhost:*", "http://localhost:*"}, make([]string, 17)} {
		b, p := nativeInputs()
		p.LoopbackRedirectURIs = uris
		if _, err := c.CreateDisabledNativeClient(context.Background(), b, p); err == nil {
			t.Fatal("invalid native policy accepted")
		}
	}
	if calls != 0 {
		t.Fatal("invalid policy caused a request")
	}
}

func TestNativeCallbackRegistration(t *testing.T) {
	b, p := nativeInputs()
	p.LoopbackRedirectURIs = []string{"http://127.0.0.1:*/callback", "http://[::1]:*/return", "http://localhost:*", "http://127.0.0.2:38123/fixed"}
	original := append([]string{}, p.LoopbackRedirectURIs...)
	desired, err := nativeClientConfiguration(b, p)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"http://127.0.0.1/callback", "http://127.0.0.2:38123/fixed", "http://[::1]/return", "http://localhost"}
	if !reflect.DeepEqual(desired.RedirectURIs, want) || !reflect.DeepEqual(p.LoopbackRedirectURIs, original) {
		t.Fatal("callback registration differs or changed caller policy")
	}
}

func TestNativeCreationAndRepair(t *testing.T) {
	b, p := nativeInputs()
	var stored map[string]any
	writes := 0
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if authRequest(w, r) {
			return
		}
		if r.Method == "POST" && r.URL.Path == "/admin/realms/tenant/clients" {
			writes++
			if stored != nil {
				w.WriteHeader(409)
				return
			}
			if json.NewDecoder(r.Body).Decode(&stored) != nil {
				t.Error("invalid native creation")
				w.WriteHeader(400)
				return
			}
			for _, key := range []string{"enabled", "serviceAccountsEnabled", "implicitFlowEnabled", "directAccessGrantsEnabled", "fullScopeAllowed"} {
				if stored[key] != false {
					t.Error("unsafe native creation", key)
				}
			}
			if stored["publicClient"] != true || stored["standardFlowEnabled"] != true || stored["id"] != b.ID {
				t.Error("native profile differs")
			}
			if _, ok := stored["secret"]; ok {
				t.Error("public client supplied a secret")
			}
			// The pinned Keycloak factory applies this default after creation.
			stored["attributes"].(map[string]any)["backchannel.logout.revoke.offline.tokens"] = "false"
			w.Header().Set("Location", "https://foreign.invalid/client/foreign")
			w.WriteHeader(201)
			return
		}
		if r.URL.Path != "/admin/realms/tenant/clients/native-id" {
			t.Error("unexpected native path")
			w.WriteHeader(500)
			return
		}
		if r.Method == "GET" {
			_ = json.NewEncoder(w).Encode(stored)
			return
		}
		if r.Method == "PUT" {
			writes++
			var update map[string]any
			if json.NewDecoder(r.Body).Decode(&update) != nil {
				t.Error("invalid native repair")
				w.WriteHeader(400)
				return
			}
			for _, key := range []string{"secret", "protocolMappers", "defaultClientScopes", "optionalClientScopes"} {
				if _, ok := update[key]; ok {
					t.Error("base update changed separate policy", key)
				}
			}
			if update["enabled"] != false {
				t.Error("native repair enabled client")
			}
			for k, v := range update {
				stored[k] = v
			}
			w.WriteHeader(204)
			return
		}
		t.Error("unexpected native operation")
		w.WriteHeader(500)
	})
	if _, err := c.CreateDisabledNativeClient(context.Background(), b, p); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateDisabledNativeClient(context.Background(), b, p); !errors.Is(err, ErrConflict) {
		t.Fatal("native conflict differs", err)
	}
	if err := c.ConfigureDisabledNativeClient(context.Background(), b, p); err != nil || writes != 3 {
		t.Fatal("converged native client was changed", err)
	}
	stored["attributes"].(map[string]any)["pkce.code.challenge.method"] = "plain"
	if _, err := c.InspectDisabledNativeClient(context.Background(), b, p); !errors.Is(err, ErrClientConfiguration) {
		t.Fatal("PKCE drift accepted", err)
	}
	p.DisplayName = ""
	p.EnableDeviceAuthorization = false
	p.LoopbackRedirectURIs = []string{"http://[::1]:*/callback"}
	if err := c.ConfigureDisabledNativeClient(context.Background(), b, p); err != nil {
		t.Fatal(err)
	}
	if writes != 4 || stored["name"] != "" || stored["attributes"].(map[string]any)["pkce.code.challenge.method"] != "S256" || stored["attributes"].(map[string]any)["oauth2.device.authorization.grant.enabled"] != "false" {
		t.Fatal("native repair differs")
	}
	if err := c.ConfigureDisabledNativeClient(context.Background(), b, p); err != nil || writes != 4 {
		t.Fatal("repeated native repair changed state", err)
	}
}

func TestNativeProfileRejectsDrift(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"flow override", func(v map[string]any) {
			v["authenticationFlowBindingOverrides"] = map[string]string{"browser": "other-flow"}
		}},
		{"null flow map", func(v map[string]any) { v["authenticationFlowBindingOverrides"] = nil }},
		{"management target", func(v map[string]any) { v["adminUrl"] = "https://other.invalid" }},
		{"front-channel logout", func(v map[string]any) { v["frontchannelLogout"] = true }},
		{"surrogate authentication", func(v map[string]any) { v["surrogateAuthRequired"] = true }},
		{"enabled", func(v map[string]any) { v["enabled"] = true }},
		{"missing enabled", func(v map[string]any) { delete(v, "enabled") }},
		{"authorization null", func(v map[string]any) { v["authorizationServicesEnabled"] = nil }},
		{"confidential", func(v map[string]any) { v["publicClient"] = false }},
		{"missing public", func(v map[string]any) { delete(v, "publicClient") }},
		{"password grant", func(v map[string]any) { v["directAccessGrantsEnabled"] = true }},
		{"implicit grant", func(v map[string]any) { v["implicitFlowEnabled"] = true }},
		{"unexpected attribute", func(v map[string]any) { v["attributes"].(map[string]string)["unknown.protocol.setting"] = "true" }},
		{"shared scope", func(v map[string]any) { v["defaultClientScopes"] = []string{"roles"} }},
		{"null origins", func(v map[string]any) { v["webOrigins"] = nil }},
		{"redirect duplicate", func(v map[string]any) { v["redirectUris"] = []string{"http://localhost:*", "http://localhost:*"} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			b, p := nativeInputs()
			record := nativeRecord()
			test.change(record)
			writes := 0
			c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if authRequest(w, r) {
					return
				}
				if r.Method != "GET" {
					writes++
				}
				_ = json.NewEncoder(w).Encode(record)
			})
			if _, err := c.InspectDisabledNativeClient(context.Background(), b, p); err == nil {
				t.Fatal("native drift accepted")
			}
			if writes != 0 {
				t.Fatal("inspection changed state")
			}
		})
	}
}

func TestNativeRepairRequiresConfirmedOwnershipAndDisablement(t *testing.T) {
	for _, fault := range []string{"foreign", "enabled", "missing-enabled", "ignored"} {
		t.Run(fault, func(t *testing.T) {
			b, p := nativeInputs()
			record := nativeRecord()
			record["name"] = "drift"
			if fault == "foreign" {
				record["attributes"].(map[string]string)["stego.owner.product"] = "other"
			}
			if fault == "enabled" {
				record["enabled"] = true
			}
			if fault == "missing-enabled" {
				delete(record, "enabled")
			}
			before, _ := json.Marshal(record)
			writes := 0
			c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if authRequest(w, r) {
					return
				}
				if r.Method == "PUT" {
					var body map[string]any
					_ = json.NewDecoder(r.Body).Decode(&body)
					writes++
					w.WriteHeader(204)
					return
				}
				_ = json.NewEncoder(w).Encode(record)
			})
			if err := c.ConfigureDisabledNativeClient(context.Background(), b, p); err == nil {
				t.Fatal("unconfirmed repair accepted")
			}
			if fault != "ignored" && writes != 0 {
				t.Fatal("unsafe client was changed")
			}
			after, _ := json.Marshal(record)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("ignored fixture write changed state")
			}
		})
	}
}

func TestNativeRepairExplicitlyRemovesFlowOverrides(t *testing.T) {
	b, p := nativeInputs()
	record := nativeRecord()
	record["authenticationFlowBindingOverrides"] = map[string]string{"browser": "foreign-flow"}
	writes := 0
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if authRequest(w, r) {
			return
		}
		if r.Method == "PUT" {
			writes++
			var update map[string]json.RawMessage
			if json.NewDecoder(r.Body).Decode(&update) != nil {
				t.Error("invalid update")
				w.WriteHeader(400)
				return
			}
			var patch map[string]string
			if json.Unmarshal(update["authenticationFlowBindingOverrides"], &patch) != nil || len(patch) != 1 {
				t.Error("flow removal patch is missing")
				w.WriteHeader(400)
				return
			}
			value, present := patch["browser"]
			if !present || value != "" {
				t.Error("flow removal must be explicit")
				w.WriteHeader(400)
				return
			}
			record["authenticationFlowBindingOverrides"] = map[string]string{}
			w.WriteHeader(204)
			return
		}
		_ = json.NewEncoder(w).Encode(record)
	})
	if err := c.ConfigureDisabledNativeClient(context.Background(), b, p); err != nil || writes != 1 {
		t.Fatal("flow override repair failed", err)
	}
}

// Keycloak patches attribute entries and removes explicit empty values.
func TestNativeRepairRemovesUnwantedAttributes(t *testing.T) {
	for _, ignoreRemoval := range []bool{false, true} {
		b, p := nativeInputs()
		record := nativeRecord()
		attributes := record["attributes"].(map[string]string)
		attributes["unknown.protocol.setting"] = "true"
		writes := 0
		c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			if authRequest(w, r) {
				return
			}
			if r.Method == http.MethodGet {
				_ = json.NewEncoder(w).Encode(record)
				return
			}
			if r.Method != http.MethodPut {
				t.Error("unexpected native repair method")
				w.WriteHeader(500)
				return
			}
			var update struct {
				Attributes map[string]string `json:"attributes"`
			}
			if json.NewDecoder(r.Body).Decode(&update) != nil {
				t.Error("invalid native attribute patch")
				w.WriteHeader(400)
				return
			}
			writes++
			for key, value := range update.Attributes {
				if value == "" {
					if !ignoreRemoval {
						delete(attributes, key)
					}
				} else {
					attributes[key] = value
				}
			}
			w.WriteHeader(204)
		})
		err := c.ConfigureDisabledNativeClient(context.Background(), b, p)
		if ignoreRemoval {
			if !errors.Is(err, ErrClientConfiguration) {
				t.Fatal("ignored attribute removal accepted", err)
			}
		} else {
			if err != nil {
				t.Fatal(err)
			}
			if err = c.ConfigureDisabledNativeClient(context.Background(), b, p); err != nil {
				t.Fatal(err)
			}
		}
		if writes != 1 {
			t.Fatal("native attribute repair was repeated", writes)
		}
	}
}
