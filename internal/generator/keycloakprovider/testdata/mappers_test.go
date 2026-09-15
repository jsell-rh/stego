package keycloak

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func newMapperFixture(t *testing.T) (*Client, *roleFixture) {
	c, f := newRoleFixture(t)
	f.scopes["default-client-scopes"] = []assignedScope{}
	f.scopes["optional-client-scopes"] = []assignedScope{}
	f.mappers = []protocolMapper{{ID: "old-mapper", Name: "old", Protocol: "openid-connect", Type: "oidc-hardcoded-claim-mapper", Config: map[string]string{"claim.name": "unwanted", "claim.value": "private"}}}
	return c, f
}

func TestTokenMapperPolicy(t *testing.T) {
	c, f := newMapperFixture(t)
	owner, target := roleBindings()
	p := TokenClaimsPolicy{AudienceClients: []ClientBinding{target}, CustomAudiences: []string{"urn:warehouse"}, ClientRoles: []ClientRoleClaim{{Client: target, Claim: "catalog.roles"}}, RealmRolesClaim: "realm_access.roles", ClientMetadata: true}
	if err := c.InspectTokenMappers(context.Background(), owner, p); !errors.Is(err, ErrMapperPolicy) {
		t.Fatal("unexpected initial mapper inspection", err)
	}
	if len(f.writes) != 0 {
		t.Fatal("inspection changed state")
	}
	if err := c.ReconcileTokenMappers(context.Background(), owner, p); err != nil {
		t.Fatal(err)
	}
	if len(f.mappers) != 8 {
		t.Fatal("mapper count differs", len(f.mappers))
	}
	counts := map[string]int{}
	ids := map[string]string{}
	for _, m := range f.mappers {
		counts[m.Type]++
		ids[m.Name] = m.ID
		if m.Config["access.token.claim"] != "true" || m.Config["introspection.token.claim"] != "false" {
			t.Fatal("access claim settings differ")
		}
		if m.Type == "oidc-usermodel-client-role-mapper" && (m.Config["claim.name"] != "catalog.roles" || m.Config["usermodel.clientRoleMapping.clientId"] != "catalog" || m.Config["multivalued"] != "true") {
			t.Fatal("client role mapper differs")
		}
		if m.Type == "oidc-usermodel-realm-role-mapper" && m.Config["claim.name"] != "realm_access.roles" {
			t.Fatal("realm role claim differs")
		}
		if m.Type != "oidc-sub-mapper" && (m.Config["id.token.claim"] != "false" || m.Config["userinfo.token.claim"] != "false" || m.Config["lightweight.claim"] != "false") {
			t.Fatal("unexpected token delivery")
		}
	}
	if counts["oidc-sub-mapper"] != 1 || counts["oidc-audience-mapper"] != 2 || counts["oidc-usersessionmodel-note-mapper"] != 3 {
		t.Fatal("mapper types differ")
	}
	count := len(f.writes)
	if err := c.ReconcileTokenMappers(context.Background(), owner, p); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != count {
		t.Fatal("converged mapper caused a write")
	}
	// A provider setting outside the requested map must not survive repair.
	for i := range f.mappers {
		if f.mappers[i].Type == "oidc-usermodel-client-role-mapper" {
			f.mappers[i].Config["aggregate.attrs"] = "true"
		}
	}
	if err := c.InspectTokenMappers(context.Background(), owner, p); !errors.Is(err, ErrMapperPolicy) {
		t.Fatal("extra configuration accepted", err)
	}
	if err := c.ReconcileTokenMappers(context.Background(), owner, p); err != nil {
		t.Fatal(err)
	}
	for _, m := range f.mappers {
		if m.Type == "oidc-usermodel-client-role-mapper" {
			if m.ID == ids[m.Name] {
				t.Fatal("changed mapper was retained")
			}
		} else if m.ID != ids[m.Name] {
			t.Fatal("unrelated mapper was replaced")
		}
	}
	value := f.clients[owner.ID]
	value.Enabled = true
	f.clients[owner.ID] = value
	if err := c.InspectTokenMappers(context.Background(), owner, p); err != nil {
		t.Fatal(err)
	}
	count = len(f.writes)
	if err := c.ReconcileTokenMappers(context.Background(), owner, p); err == nil {
		t.Fatal("enabled mapper mutation accepted")
	}
	if len(f.writes) != count {
		t.Fatal("enabled client was changed")
	}
}

func TestTokenMapperValidation(t *testing.T) {
	_, target := roleBindings()
	for _, claim := range []string{"sub", "sub.roles", "aud", "authorization.roles", "client_id", "catalog.__proto__.roles", "catalog.constructor", "roles..name", "roles.$name", "roles\\.name", "", "roles."} {
		if err := (TokenClaimsPolicy{ClientRoles: []ClientRoleClaim{{Client: target, Claim: claim}}}).validate(); err == nil {
			t.Fatal("invalid claim accepted", claim)
		}
	}
	for _, p := range []TokenClaimsPolicy{
		{CustomAudiences: []string{"duplicate", "duplicate"}},
		{AudienceClients: []ClientBinding{target}, CustomAudiences: []string{target.ClientID}},
		{CustomAudiences: make([]string, MaxTokenAudiences+1)},
		{RealmRolesClaim: "catalog", ClientRoles: []ClientRoleClaim{{Client: target, Claim: "catalog.roles"}}},
		{RealmRolesClaim: "catalog.roles", ClientRoles: []ClientRoleClaim{{Client: target, Claim: "catalog"}}},
		{RealmRolesClaim: "catalog.roles", ClientRoles: []ClientRoleClaim{{Client: target, Claim: "catalog.roles"}}},
	} {
		if p.validate() == nil {
			t.Fatal("invalid claim policy accepted")
		}
	}
}

func TestTokenMapperFailureStopsAdditions(t *testing.T) {
	for _, test := range []struct {
		name     string
		change   func(*roleFixture)
		want     error
		noWrites bool
	}{
		{"ignored removal", func(f *roleFixture) { f.ignoreDelete = true }, ErrMapperPolicy, false},
		{"shared scope returns", func(f *roleFixture) { f.scopeOnMapperDelete = true }, ErrMapperPolicy, false},
		{"null response", func(f *roleFixture) { f.malformedMappers = true }, ErrResponse, true},
		{"path escape", func(f *roleFixture) { f.mappers[0].ID = "../foreign" }, ErrResponse, true},
		{"duplicate ID", func(f *roleFixture) { m := f.mappers[0]; m.Name = "different"; f.mappers = append(f.mappers, m) }, ErrResponse, true},
		{"duplicate name", func(f *roleFixture) { m := f.mappers[0]; m.ID = "different"; f.mappers = append(f.mappers, m) }, ErrResponse, true},
		{"mapper limit", func(f *roleFixture) { f.mappers = make([]protocolMapper, MaxProtocolMappers+1) }, ErrResponse, true},
		{"target binding differs", func(f *roleFixture) {
			v := f.clients["target"]
			v.Attributes = map[string]string{"product.owner": "another"}
			f.clients["target"] = v
		}, ErrOwnership, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, f := newMapperFixture(t)
			owner, target := roleBindings()
			test.change(f)
			err := c.ReconcileTokenMappers(context.Background(), owner, TokenClaimsPolicy{AudienceClients: []ClientBinding{target}})
			if !errors.Is(err, test.want) {
				t.Fatal("mapper failure differs", err)
			}
			if test.noWrites && len(f.writes) != 0 {
				t.Fatal("invalid state caused a write")
			}
			for _, write := range f.writes {
				if strings.HasPrefix(write, "POST ") {
					t.Fatal("addition followed incomplete removal")
				}
			}
			if f.clients[owner.ID].Enabled {
				t.Fatal("failure enabled client")
			}
		})
	}
}

func TestTokenMapperPartialFailureCanRecover(t *testing.T) {
	for _, ignored := range []bool{false, true} {
		t.Run(map[bool]string{false: "unavailable", true: "ignored"}[ignored], func(t *testing.T) {
			c, f := newMapperFixture(t)
			owner, _ := roleBindings()
			f.ignoreAdd = ignored
			f.failAdd = !ignored
			if err := c.ReconcileTokenMappers(context.Background(), owner, TokenClaimsPolicy{}); err == nil {
				t.Fatal("unconfirmed creation accepted")
			}
			if len(f.mappers) != 0 || f.clients[owner.ID].Enabled {
				t.Fatal("failed creation retained unwanted claims or enabled client")
			}
			f.ignoreAdd = false
			f.failAdd = false
			if err := c.ReconcileTokenMappers(context.Background(), owner, TokenClaimsPolicy{}); err != nil {
				t.Fatal(err)
			}
			if len(f.mappers) != 1 || f.mappers[0].Type != "oidc-sub-mapper" {
				t.Fatal("subject mapper is missing")
			}
		})
	}
}
