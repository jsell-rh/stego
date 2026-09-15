package keycloak

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

// This fixture is independent of the configuration builder. It includes the
// optional-field behavior of a full Keycloak client response.
func disabledServiceAccountRecord() map[string]any {
	return map[string]any{
		"frontchannelLogout": false, "surrogateAuthRequired": false, "authenticationFlowBindingOverrides": map[string]string{},
		"id": "stable-id", "clientId": "catalog", "name": "Catalog worker", "protocol": "openid-connect", "clientAuthenticatorType": "client-secret",
		"enabled": false, "publicClient": false, "bearerOnly": false, "consentRequired": false, "serviceAccountsEnabled": true,
		"standardFlowEnabled": false, "implicitFlowEnabled": false, "directAccessGrantsEnabled": false, "fullScopeAllowed": false,
		"redirectUris": []string{}, "webOrigins": []string{}, "defaultClientScopes": []string{"service_account"}, "optionalClientScopes": []string{},
		"attributes": map[string]string{"stego.owner.product": "object-1", "access.token.lifespan": "300", "client_credentials.use_refresh_token": "false", "oauth2.device.authorization.grant.enabled": "false", "oidc.ciba.grant.enabled": "false", "standard.token.exchange.enabled": "false", "oauth2.jwt.authorization.grant.enabled": "false", "external.token.enabled": "false"},
	}
}
func serviceAccountInputs() (ClientBinding, ServiceAccountPolicy) {
	return ClientBinding{ID: "stable-id", ClientID: "catalog", Attributes: map[string]string{"stego.owner.product": "object-1"}}, ServiceAccountPolicy{DisplayName: "Catalog worker", AccessTokenLifetimeSeconds: 300}
}
func TestCreateDisabledServiceAccount(t *testing.T) {
	for _, application := range []struct{ name, key, value string }{{"catalog", "stego.owner.product", "object-1"}, {"batch-worker", "stego.owner.pipeline", "run-2"}} {
		t.Run(application.name, func(t *testing.T) {
			b, p := serviceAccountInputs()
			b.ClientID = application.name
			b.Attributes = map[string]string{application.key: application.value}
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
						t.Error("invalid creation body")
						w.WriteHeader(400)
						return
					}
					for _, key := range []string{"enabled", "publicClient", "bearerOnly", "consentRequired", "standardFlowEnabled", "implicitFlowEnabled", "directAccessGrantsEnabled", "authorizationServicesEnabled", "fullScopeAllowed"} {
						if stored[key] != false {
							t.Errorf("unsafe creation flag: %s", key)
						}
					}
					if stored["id"] != b.ID || stored["clientId"] != b.ClientID || stored["serviceAccountsEnabled"] != true || stored["clientAuthenticatorType"] != "client-secret" {
						t.Error("creation identity or authentication differs")
					}
					if _, exists := stored["secret"]; exists {
						t.Error("creation supplied a secret")
					}
					// A location outside the expected identity must never be followed.
					w.Header().Set("Location", "https://other.invalid/admin/clients/foreign")
					w.WriteHeader(201)
					return
				}
				if r.Method == "GET" && r.URL.Path == "/admin/realms/tenant/clients/stable-id" && stored != nil {
					_ = json.NewEncoder(w).Encode(stored)
					return
				}
				t.Error("unexpected request", r.Method, r.URL.Path)
				w.WriteHeader(500)
			})
			if record, err := c.CreateDisabledServiceAccount(context.Background(), b, p); err != nil || record.Enabled {
				t.Fatal("disabled creation failed", err)
			}
			if _, err := c.CreateDisabledServiceAccount(context.Background(), b, p); !errors.Is(err, ErrConflict) {
				t.Fatal("creation conflict was not preserved", err)
			}
			if writes != 2 {
				t.Fatal("creation was replayed")
			}
			if err := c.ConfigureDisabledServiceAccount(context.Background(), b, p); err != nil {
				t.Fatal("converged configuration failed", err)
			}
		})
	}
}
func TestCreationRequiresStoredConfirmation(t *testing.T) {
	for _, fault := range []string{"missing", "foreign-owner", "enabled", "lost-response"} {
		t.Run(fault, func(t *testing.T) {
			b, p := serviceAccountInputs()
			writes := 0
			record := disabledServiceAccountRecord()
			if fault == "foreign-owner" {
				record["attributes"].(map[string]string)["stego.owner.product"] = "someone-else"
			}
			if fault == "enabled" {
				record["enabled"] = true
			}
			c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if authRequest(w, r) {
					return
				}
				if r.Method == "POST" {
					writes++
					if fault == "lost-response" {
						w.WriteHeader(500)
					} else {
						w.WriteHeader(201)
					}
					return
				}
				if r.Method != "GET" {
					t.Error("unexpected mutation")
					w.WriteHeader(500)
					return
				}
				if fault == "missing" {
					w.WriteHeader(404)
					return
				}
				_ = json.NewEncoder(w).Encode(record)
			})
			if _, err := c.CreateDisabledServiceAccount(context.Background(), b, p); err == nil {
				t.Fatal("unconfirmed creation succeeded")
			}
			if writes != 1 {
				t.Fatal("uncertain creation was retried")
			}
			if fault == "lost-response" {
				if _, err := c.InspectDisabledServiceAccount(context.Background(), b, p); err != nil {
					t.Fatal("stable identity did not recover creation", err)
				}
			}
		})
	}
}
func TestDisabledConfigurationChecks(t *testing.T) {
	changes := map[string]func(map[string]any){
		"enabled":                 func(v map[string]any) { v["enabled"] = true },
		"missing enabled":         func(v map[string]any) { delete(v, "enabled") },
		"null enabled":            func(v map[string]any) { v["enabled"] = nil },
		"missing public flag":     func(v map[string]any) { delete(v, "publicClient") },
		"authorization server":    func(v map[string]any) { v["authorizationServicesEnabled"] = true },
		"null authorization flag": func(v map[string]any) { v["authorizationServicesEnabled"] = nil },
		"optional scope":          func(v map[string]any) { v["optionalClientScopes"] = []string{"offline_access"} },
		"default scope":           func(v map[string]any) { v["defaultClientScopes"] = []string{"roles"} },
		"missing scopes":          func(v map[string]any) { delete(v, "defaultClientScopes") },
		"redirect":                func(v map[string]any) { v["redirectUris"] = []string{"https://example.com"} },
		"refresh token": func(v map[string]any) {
			v["attributes"].(map[string]string)["client_credentials.use_refresh_token"] = "true"
		},
		"wrong lifetime": func(v map[string]any) { v["attributes"].(map[string]string)["access.token.lifespan"] = "3600" },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			b, p := serviceAccountInputs()
			record := disabledServiceAccountRecord()
			change(record)
			c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if authRequest(w, r) {
					return
				}
				if r.Method != "GET" {
					t.Error("inspection changed client")
				}
				_ = json.NewEncoder(w).Encode(record)
			})
			if _, err := c.InspectDisabledServiceAccount(context.Background(), b, p); err == nil {
				t.Fatal("unsafe or incomplete configuration accepted")
			}
		})
	}
}
func TestConfigureRequiresOwnershipAndDisablement(t *testing.T) {
	for _, fault := range []string{"foreign-owner", "enabled", "missing enabled", "null enabled", "repair", "ignored-write"} {
		t.Run(fault, func(t *testing.T) {
			b, p := serviceAccountInputs()
			record := disabledServiceAccountRecord()
			record["standardFlowEnabled"] = true
			writes := 0
			switch fault {
			case "foreign-owner":
				record["attributes"].(map[string]string)["stego.owner.product"] = "someone-else"
			case "enabled":
				record["enabled"] = true
			case "missing enabled":
				delete(record, "enabled")
			case "null enabled":
				record["enabled"] = nil
			}
			c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if authRequest(w, r) {
					return
				}
				if r.Method == "GET" {
					_ = json.NewEncoder(w).Encode(record)
					return
				}
				if r.Method != "PUT" {
					t.Error("unexpected mutation")
					w.WriteHeader(500)
					return
				}
				writes++
				var update map[string]any
				if json.NewDecoder(r.Body).Decode(&update) != nil {
					t.Error("invalid update")
				}
				for _, key := range []string{"secret", "protocolMappers", "defaultClientScopes", "optionalClientScopes"} {
					if _, exists := update[key]; exists {
						t.Error("update changed separate policy", key)
					}
				}
				if update["enabled"] != false {
					t.Error("repair enabled client")
				}
				if fault != "ignored-write" {
					for key, value := range update {
						record[key] = value
					}
				}
				w.WriteHeader(204)
			})
			err := c.ConfigureDisabledServiceAccount(context.Background(), b, p)
			if fault == "repair" {
				if err != nil || writes != 1 {
					t.Fatal("repair failed", err, writes)
				}
				if err = c.ConfigureDisabledServiceAccount(context.Background(), b, p); err != nil || writes != 1 {
					t.Fatal("converged repair wrote again", err, writes)
				}
			} else if err == nil {
				t.Fatal("unsafe or unconfirmed repair succeeded")
			}
			if fault != "repair" && fault != "ignored-write" && writes != 0 {
				t.Fatal("repair wrote without ownership and disablement")
			}
		})
	}
}
func TestServiceAccountPolicyRejectsInvalidInput(t *testing.T) {
	calls := 0
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(500) })
	for _, lifetime := range []int{-1, 0, 3601} {
		b, p := serviceAccountInputs()
		p.AccessTokenLifetimeSeconds = lifetime
		if _, err := c.CreateDisabledServiceAccount(context.Background(), b, p); err == nil {
			t.Fatal("invalid lifetime accepted")
		}
	}
	for _, key := range []string{"access.token.lifespan", "access.token.signed.response.alg", "use.jwks.url", "product.owner", "stego.owner."} {
		b, p := serviceAccountInputs()
		b.Attributes[key] = "value"
		if _, err := c.CreateDisabledServiceAccount(context.Background(), b, p); err == nil {
			t.Fatal("protocol setting or invalid ownership key accepted", key)
		}
	}
	if calls != 0 {
		t.Fatal("invalid policy caused a request")
	}
}
