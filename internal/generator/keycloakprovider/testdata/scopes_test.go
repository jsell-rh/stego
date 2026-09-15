package keycloak

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestClientScopePolicy(t *testing.T) {
	c, f := newRoleFixture(t)
	owner, target := roleBindings()
	p := RolePolicy{Realm: []string{"base"}, Clients: []ClientRoleGrant{{Client: target, Names: []string{"read"}}}}
	if err := c.InspectClientScopes(context.Background(), owner, p); !errors.Is(err, ErrScopePolicy) {
		t.Fatal("unexpected initial scope result", err)
	}
	if len(f.writes) != 0 {
		t.Fatal("inspection changed state")
	}
	if err := c.ReconcileClientScopes(context.Background(), owner, p); err != nil {
		t.Fatal(err)
	}
	if len(f.scopes["default-client-scopes"])+len(f.scopes["optional-client-scopes"]) != 0 {
		t.Fatal("shared scopes remain")
	}
	state := f.current["scope:worker"]
	if len(state.Realm) != 1 || state.Realm[0].ID != "realm-base" || len(state.Clients["target"]) != 1 || state.Clients["target"][0].ID != "read-id" || len(state.Clients["foreign"]) != 0 {
		t.Fatal("scope policy differs")
	}
	if len(f.current["service-user"].Realm) != 1 || f.current["service-user"].Realm[0].ID != "realm-old" || len(f.groups["service-user"]) != 1 {
		t.Fatal("scope operation changed user access")
	}
	adding := false
	for _, write := range f.writes {
		if strings.HasPrefix(write, "POST ") {
			adding = true
		} else if adding {
			t.Fatal("removal followed addition")
		}
		if !strings.HasPrefix(write, "DELETE clients/worker/") && !strings.HasPrefix(write, "POST clients/worker/scope-mappings/") {
			t.Fatal("scope write escaped client", write)
		}
	}
	count := len(f.writes)
	if err := c.ReconcileClientScopes(context.Background(), owner, p); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != count {
		t.Fatal("converged scope policy caused a write")
	}
	v := f.clients[owner.ID]
	v.Enabled = true
	f.clients[owner.ID] = v
	if err := c.InspectClientScopes(context.Background(), owner, p); err != nil {
		t.Fatal("enabled inspection failed", err)
	}
	if err := c.ReconcileClientScopes(context.Background(), owner, p); !errors.Is(err, ErrScopePolicy) {
		t.Fatal("enabled mutation accepted", err)
	}
	if len(f.writes) != count {
		t.Fatal("enabled client was changed")
	}
}

func TestClientScopeFailureStopsGrants(t *testing.T) {
	for _, test := range []struct {
		name     string
		change   func(*roleFixture)
		want     error
		noWrites bool
	}{
		{"hidden scope inventory", func(f *roleFixture) { f.hiddenScopes = true }, ErrScopePolicy, true},
		{"ignored scope removal", func(f *roleFixture) { f.ignoreScopeDelete = true }, ErrScopePolicy, false},
		{"ignored role removal", func(f *roleFixture) { f.ignoreDelete = true }, ErrScopePolicy, false},
		{"hidden effective role", func(f *roleFixture) { f.inherited = true }, ErrScopePolicy, false},
		{"invalid scope response", func(f *roleFixture) { f.malformedScopes = true }, ErrResponse, true},
		{"repeated scope", func(f *roleFixture) { f.scopes["optional-client-scopes"] = f.scopes["default-client-scopes"] }, ErrResponse, true},
		{"scope path escape", func(f *roleFixture) { f.scopes["default-client-scopes"][0].ID = "../foreign" }, ErrResponse, true},
		{"scope limit", func(f *roleFixture) {
			f.scopes["default-client-scopes"] = make([]assignedScope, MaxAssignedClientScopes+1)
		}, ErrResponse, true},
		{"ownership change", func(f *roleFixture) { f.changeOwnerOnDelete = true }, ErrOwnership, false},
		{"full scope", func(f *roleFixture) { v := f.clients["worker"]; v.FullScopeAllowed = true; f.clients["worker"] = v }, ErrScopePolicy, true},
		{"composite desired role", func(f *roleFixture) { v := f.roles["target/read"]; v.Composite = true; f.roles["target/read"] = v }, ErrRolePolicy, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, f := newRoleFixture(t)
			test.change(f)
			owner, target := roleBindings()
			err := c.ReconcileClientScopes(context.Background(), owner, RolePolicy{Clients: []ClientRoleGrant{{Client: target, Names: []string{"read"}}}})
			if !errors.Is(err, test.want) {
				t.Fatal("scope failure differs", err)
			}
			if test.noWrites && len(f.writes) != 0 {
				t.Fatal("invalid state caused a write")
			}
			for _, write := range f.writes {
				if strings.HasPrefix(write, "POST ") {
					t.Fatal("grant followed incomplete removal")
				}
			}
			if f.clients[owner.ID].Enabled {
				t.Fatal("failure enabled client")
			}
		})
	}
}

func TestClientScopeFailedAdditionCanRecover(t *testing.T) {
	for _, ignored := range []bool{false, true} {
		t.Run(map[bool]string{false: "unavailable", true: "ignored"}[ignored], func(t *testing.T) {
			c, f := newRoleFixture(t)
			owner, target := roleBindings()
			f.ignoreAdd = ignored
			f.failAdd = !ignored
			p := RolePolicy{Clients: []ClientRoleGrant{{Client: target, Names: []string{"read"}}}}
			if err := c.ReconcileClientScopes(context.Background(), owner, p); err == nil {
				t.Fatal("unconfirmed addition accepted")
			}
			if f.clients[owner.ID].Enabled || len(f.current["scope:worker"].Realm) != 0 || len(f.current["scope:worker"].Clients["foreign"]) != 0 {
				t.Fatal("partial failure retained excess scope or enabled client")
			}
			f.ignoreAdd = false
			f.failAdd = false
			if err := c.ReconcileClientScopes(context.Background(), owner, p); err != nil {
				t.Fatal("retry failed", err)
			}
			if err := c.ReconcileClientScopes(context.Background(), owner, RolePolicy{}); err != nil {
				t.Fatal("empty policy failed", err)
			}
			if err := c.InspectClientScopes(context.Background(), owner, RolePolicy{}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestClientScopeVisibilityRequiresDirectRead(t *testing.T) {
	c, f := newRoleFixture(t)
	owner, _ := roleBindings()
	f.scopes["default-client-scopes"] = []assignedScope{}
	f.scopes["optional-client-scopes"] = []assignedScope{}
	f.denyScopeProbe = true
	if err := c.ReconcileClientScopes(context.Background(), owner, RolePolicy{}); err == nil {
		t.Fatal("scope visibility was not proved")
	}
	if len(f.writes) != 0 {
		t.Fatal("hidden scope state caused a write")
	}
}
