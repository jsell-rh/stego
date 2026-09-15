package keycloak

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func serviceTokenFixture(t *testing.T) (*rsa.PrivateKey, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	document, _ := json.Marshal(map[string]any{"keys": []any{map[string]string{"kid": "proof-key", "kty": "RSA", "alg": "RS256", "use": "sig", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": "AQAB"}}})
	return key, document
}
func signedServiceToken(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	body, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT","kid":"proof-key"}`))
	input := header + "." + base64.RawURLEncoding.EncodeToString(body)
	digest := sha256.Sum256([]byte(input))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(signature)
}
func serviceTokenClaims(issuer string) map[string]any {
	now := time.Now().Unix()
	return map[string]any{"iss": issuer, "sub": "saved-subject", "azp": "worker", "aud": []string{"catalog", "urn:batch"}, "iat": now, "exp": now + 300, "catalog": map[string]any{"roles": []string{"read"}}}
}
func serviceTokenPolicy() ServiceAccountTokenPolicy {
	return ServiceAccountTokenPolicy{Subject: "saved-subject", AccessTokenLifetimeSeconds: 300, Audiences: []string{"catalog", "urn:batch"}, RoleClaims: map[string][]string{"catalog.roles": {"read"}}}
}

func TestServiceAccountSignedTokenPolicy(t *testing.T) {
	key, keys := serviceTokenFixture(t)
	issuer := "https://issuer.invalid/realms/tenant"
	for _, fault := range []string{"valid", "wrong issuer", "wrong subject", "wrong client", "extra audience", "duplicate audience", "non-string audience", "extra role", "scalar role", "missing role", "null role", "fractional time", "string time", "long lifetime", "short lifetime", "old token", "wrong expiry response", "bad signature", "wrong key"} {
		t.Run(fault, func(t *testing.T) {
			claims := serviceTokenClaims(issuer)
			p := serviceTokenPolicy()
			expires := 300
			document := keys
			switch fault {
			case "wrong issuer":
				claims["iss"] = "https://other.invalid"
			case "wrong subject":
				claims["sub"] = "other-subject"
			case "wrong client":
				claims["azp"] = "other-client"
			case "extra audience":
				claims["aud"] = []string{"catalog", "urn:batch", "other"}
			case "duplicate audience":
				claims["aud"] = []string{"catalog", "urn:batch", "catalog"}
			case "non-string audience":
				claims["aud"] = []any{"catalog", 1}
			case "extra role":
				claims["catalog"] = map[string]any{"roles": []string{"read", "admin"}}
			case "scalar role":
				claims["catalog"] = map[string]any{"roles": "read"}
			case "missing role":
				delete(claims, "catalog")
			case "null role":
				claims["catalog"] = map[string]any{"roles": nil}
			case "fractional time":
				claims["iat"] = float64(time.Now().Unix()) - 0.5
			case "string time":
				claims["iat"] = "123"
			case "long lifetime":
				claims["exp"] = claims["iat"].(int64) + 301
			case "short lifetime":
				claims["exp"] = claims["iat"].(int64) + 299
			case "old token":
				claims["iat"] = time.Now().Unix() - 20
				claims["exp"] = time.Now().Unix() + 280
			case "wrong expiry response":
				expires = 290
			case "wrong key":
				document = []byte(`{"keys":[]}`)
			}
			token := signedServiceToken(t, key, claims)
			if fault == "bad signature" {
				parts := strings.Split(token, ".")
				parts[2] = base64.RawURLEncoding.EncodeToString(make([]byte, 256))
				token = strings.Join(parts, ".")
			}
			err := verifyServiceAccountClaims(issuer, "worker", p, token, document, expires, time.Now())
			if (err == nil) != (fault == "valid") {
				t.Fatal("token policy result differs", err)
			}
		})
	}
	// No effective roles can produce an absent nested claim. Null is not absence.
	p := serviceTokenPolicy()
	p.RoleClaims["catalog.roles"] = []string{}
	for _, value := range []any{"absent", map[string]any{"roles": []string{}}, map[string]any{"roles": nil}} {
		claims := serviceTokenClaims(issuer)
		claims["catalog"] = value
		if value == "absent" {
			delete(claims, "catalog")
		}
		token := signedServiceToken(t, key, claims)
		err := verifyServiceAccountClaims(issuer, "worker", p, token, keys, 300, time.Now())
		if object, ok := value.(map[string]any); ok && object["roles"] == nil {
			if err == nil {
				t.Fatal("null role claim accepted")
			}
		} else if err != nil {
			t.Fatal("empty role policy rejected", err)
		}
	}
}

func TestServiceAccountTokenProtocolAndOwnership(t *testing.T) {
	key, keys := serviceTokenFixture(t)
	for _, fault := range []string{"valid", "refresh token", "ID token", "wrong type", "public client", "disabled client", "missing subject", "changed owner", "changed owner after grant", "wrong subject"} {
		t.Run(fault, func(t *testing.T) {
			b := ClientBinding{ID: "owned", ClientID: "worker", Attributes: map[string]string{"stego.owner.product": "catalog"}}
			p := serviceTokenPolicy()
			if fault == "wrong subject" {
				p.Subject = "other"
			}
			credentialReads, grants, reads := 0, 0, 0
			var c *Client
			c, _ = testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/token") && r.FormValue("client_id") == "worker" {
					grants++
					if r.Method != "POST" || r.Form.Get("grant_type") != "client_credentials" || r.Form.Get("client_secret") != "private-client-secret" {
						t.Error("invalid proof grant request")
						w.WriteHeader(400)
						return
					}
					grant := map[string]any{"access_token": signedServiceToken(t, key, serviceTokenClaims(c.Issuer())), "token_type": "Bearer", "expires_in": 300}
					if fault == "refresh token" {
						grant["refresh_token"] = "must-not-return"
					}
					if fault == "ID token" {
						grant["id_token"] = "must-not-return"
					}
					if fault == "wrong type" {
						grant["token_type"] = "MAC"
					}
					_ = json.NewEncoder(w).Encode(grant)
					return
				}
				if authRequest(w, r) {
					return
				}
				switch r.URL.Path {
				case "/admin/realms/tenant/users/saved-subject", "/admin/realms/tenant/users/other":
					if fault == "missing subject" {
						w.WriteHeader(404)
					} else {
						_ = json.NewEncoder(w).Encode(map[string]any{"id": p.Subject, "enabled": true})
					}
				case "/admin/realms/tenant/clients/owned":
					reads++
					attributes := b.Attributes
					if (fault == "changed owner" && reads > 1) || (fault == "changed owner after grant" && reads > 2) {
						attributes = map[string]string{"stego.owner.product": "other"}
					}
					_ = json.NewEncoder(w).Encode(ClientRepresentation{ID: b.ID, ClientID: b.ClientID, Protocol: "openid-connect", Enabled: fault != "disabled client", PublicClient: fault == "public client", ServiceAccountsEnabled: true, Attributes: attributes})
				case "/admin/realms/tenant/clients/owned/client-secret":
					credentialReads++
					_ = json.NewEncoder(w).Encode(map[string]string{"value": "private-client-secret"})
				case "/realms/tenant/protocol/openid-connect/certs":
					_, _ = w.Write(keys)
				default:
					t.Error("proof request escaped expected endpoints")
					w.WriteHeader(500)
				}
			})
			secret, err := c.VerifiedServiceAccountSecret(context.Background(), b, p)
			if (err == nil && secret.Reveal() != "private-client-secret") || (err != nil && secret.Reveal() != "") {
				t.Fatal("verified credential boundary differs")
			}
			if (err == nil) != (fault == "valid") {
				t.Fatal("proof result differs", err)
			}
			if fault == "public client" || fault == "disabled client" || fault == "missing subject" {
				if credentialReads != 0 || grants != 0 {
					t.Fatal("unsafe client released credentials")
				}
			}
			if fault == "changed owner" && (!errors.Is(err, ErrOwnership) || grants != 0) {
				t.Fatal("changed owner reached token endpoint", err)
			}
			if err != nil && (strings.Contains(err.Error(), "private-client-secret") || strings.Contains(err.Error(), "must-not-return")) {
				t.Fatal("proof error exposed credentials")
			}
		})
	}
}
