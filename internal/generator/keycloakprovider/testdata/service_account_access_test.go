package keycloak

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"
)

func newServiceAccessFixture(t *testing.T, key *rsa.PrivateKey, keys []byte) (*Client, *nativeAccessFixture, ClientBinding, ServiceAccountAccessPolicy) {
	t.Helper()
	c, f, b, _ := newNativeAccessFixture(t)
	_, target := roleBindings()
	p := ServiceAccountAccessPolicy{
		Client: ServiceAccountPolicy{DisplayName: "Batch account", AccessTokenLifetimeSeconds: 300}, Subject: "service-user",
		Roles:  RolePolicy{Realm: []string{"base"}, Clients: []ClientRoleGrant{{Client: target, Names: []string{"read", "write"}}}},
		Scopes: RolePolicy{Realm: []string{"base"}, Clients: []ClientRoleGrant{{Client: target, Names: []string{"read"}}}},
		Claims: TokenClaimsPolicy{AudienceClients: []ClientBinding{target}, ClientRoles: []ClientRoleClaim{{Client: target, Claim: "catalog.roles"}}, RealmRolesClaim: "access.realm"},
	}
	value, err := serviceAccountConfiguration(b, p.Client)
	if err != nil {
		t.Fatal(err)
	}
	value.Name = "old name"
	value.Attributes["client.secret.creation.time"] = "1700000000"
	f.clients[b.ID] = value
	base := f.intercept
	f.intercept = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/realms/tenant/protocol/openid-connect/token" && r.FormValue("client_id") == b.ClientID {
			if !f.clients[b.ID].Enabled {
				t.Error("token requested while disabled")
				w.WriteHeader(400)
				return true
			}
			now := time.Now().Unix()
			claims := map[string]any{"iss": c.Issuer(), "sub": p.Subject, "azp": b.ClientID, "aud": target.ClientID, "iat": now, "exp": now + 300, "catalog": map[string]any{"roles": []string{"read"}}, "access": map[string]any{"realm": []string{"base"}}}
			if f.fault == "wrong token role" {
				claims["catalog"] = map[string]any{"roles": []string{"write"}}
			}
			if f.fault == "wrong token subject" {
				claims["sub"] = "another-user"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": signedServiceToken(t, key, claims), "expires_in": 300, "token_type": "Bearer"})
			return true
		}
		if r.URL.Path == "/realms/tenant/protocol/openid-connect/certs" {
			_, _ = w.Write(keys)
			return true
		}
		if r.URL.Path == "/admin/realms/tenant/clients/worker/client-secret" {
			_ = json.NewEncoder(w).Encode(map[string]string{"value": "private-service-secret"})
			return true
		}
		return base(w, r)
	}
	return c, f, b, p
}

func TestServiceAccountAccessReconciliation(t *testing.T) {
	key, keys := serviceTokenFixture(t)
	c, f, b, p := newServiceAccessFixture(t, key, keys)
	for i := 0; i < MaxConcurrentOperations-1; i++ {
		c.permits <- struct{}{}
	}
	defer func() {
		for i := 0; i < MaxConcurrentOperations-1; i++ {
			<-c.permits
		}
	}()
	if err := c.ReconcileServiceAccountAccess(context.Background(), b, p); err != nil {
		t.Fatal(err)
	}
	if !f.clients[b.ID].Enabled || len(f.groups[p.Subject]) != 0 {
		t.Fatal("service access did not converge")
	}
	count := len(f.writes)
	if err := c.InspectServiceAccountAccess(context.Background(), b, p); err != nil {
		t.Fatal(err)
	}
	if err := c.ReconcileServiceAccountAccess(context.Background(), b, p); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != count {
		t.Fatal("correct service access caused administrative writes")
	}
	if f.clients[b.ID].Attributes["client.secret.creation.time"] != "1700000000" {
		t.Fatal("provider secret metadata was removed")
	}
	// A new group and a client attribute must both be repaired while disabled.
	f.groups[p.Subject] = []string{"outside-group"}
	value := f.clients[b.ID]
	value.Attributes["unknown.setting"] = "true"
	f.clients[b.ID] = value
	if err := c.ReconcileServiceAccountAccess(context.Background(), b, p); err != nil {
		t.Fatal(err)
	}
	if f.writes[count] != "PUT enabled=false" || f.writes[len(f.writes)-1] != "PUT enabled=true" {
		t.Fatal("service repair escaped disabled state")
	}
	if len(f.groups[p.Subject]) != 0 {
		t.Fatal("inherited roles survived service repair")
	}
	// The write role is granted but outside scope. Token proof must expect read.
	if len(f.current[p.Subject].Clients["target"]) != 2 || len(f.current["scope:worker"].Clients["target"]) != 1 {
		t.Fatal("grant and scope policy were combined incorrectly")
	}
}

func TestServiceAccountAccessFailures(t *testing.T) {
	key, keys := serviceTokenFixture(t)
	for _, fault := range []string{"wrong token role", "wrong token subject", "changed subject binding", "missing saved user", "ignored role removal", "unknown timestamp", "failed rollback"} {
		t.Run(fault, func(t *testing.T) {
			c, f, b, p := newServiceAccessFixture(t, key, keys)
			f.fault = fault
			switch fault {
			case "changed subject binding":
				f.serviceSubject = "another-user"
			case "missing saved user":
				delete(f.enabled, p.Subject)
			case "ignored role removal":
				f.ignoreDelete = true
			case "unknown timestamp":
				v := f.clients[b.ID]
				v.Attributes["client.secret.creation.time"] = "invalid"
				f.clients[b.ID] = v
			}
			err := c.ReconcileServiceAccountAccess(context.Background(), b, p)
			if err == nil {
				t.Fatal("incomplete service access accepted")
			}
			if fault == "failed rollback" {
				if !errors.Is(err, ErrAccessDisablementUnconfirmed) {
					t.Fatal("failed service cleanup was hidden", err)
				}
			} else if f.clients[b.ID].Enabled || errors.Is(err, ErrAccessDisablementUnconfirmed) {
				t.Fatal("failed service access was not disabled", err)
			}
			if fault == "changed subject binding" || fault == "missing saved user" {
				if len(f.writes) != 0 {
					t.Fatal("unconfirmed subject caused a write")
				}
			}
		})
	}
}

func TestServiceAccountTokenFailureRevokesConvergedAccess(t *testing.T) {
	key, keys := serviceTokenFixture(t)
	c, f, b, p := newServiceAccessFixture(t, key, keys)
	if err := c.ReconcileServiceAccountAccess(context.Background(), b, p); err != nil {
		t.Fatal(err)
	}
	f.writes = nil
	f.fault = "wrong token subject"
	if err := c.ReconcileServiceAccountAccess(context.Background(), b, p); !errors.Is(err, ErrTokenPolicy) {
		t.Fatal("wrong issued subject was accepted", err)
	}
	if f.clients[b.ID].Enabled || len(f.writes) != 1 || f.writes[0] != "PUT enabled=false" {
		t.Fatal("failed token proof did not revoke access directly")
	}
}

func TestServiceAccountAccessRemainsDisabled(t *testing.T) {
	key, keys := serviceTokenFixture(t)
	c, f, b, p := newServiceAccessFixture(t, key, keys)
	value := f.clients[b.ID]
	value.Enabled = true
	f.clients[b.ID] = value
	base := f.intercept
	f.intercept = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/admin/realms/tenant/clients/worker/client-secret" || (r.URL.Path == "/realms/tenant/protocol/openid-connect/token" && r.FormValue("client_id") == b.ClientID) {
			t.Error("disabled repair requested a credential or token")
			w.WriteHeader(500)
			return true
		}
		return base(w, r)
	}
	if err := c.ReconcileDisabledServiceAccountAccess(context.Background(), b, p); err != nil {
		t.Fatal(err)
	}
	if f.clients[b.ID].Enabled || f.enabledOnce {
		t.Fatal("disabled repair enabled account")
	}
	if len(f.writes) == 0 || f.writes[0] != "PUT enabled=false" {
		t.Fatal("disabled repair did not first disable the account")
	}
	count := len(f.writes)
	if err := c.ReconcileDisabledServiceAccountAccess(context.Background(), b, p); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != count {
		t.Fatal("converged disabled policy caused writes")
	}
}
