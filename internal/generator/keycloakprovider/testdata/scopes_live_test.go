package keycloak

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func testLiveScopePolicy(t *testing.T, c, scopeAdmin *Client, ctx context.Context, owner, target ClientBinding, p RolePolicy) {
	t.Helper()
	request := func(method, path string, body []byte, status int) []byte {
		t.Helper()
		response, err := c.admin(ctx, method, path, body)
		if err != nil || response.StatusCode != status {
			t.Fatal("real scope fixture request failed", method, status, err)
		}
		return response.Body
	}
	raw := request(http.MethodGet, "/client-scopes", nil, http.StatusOK)
	var scopes []assignedScope
	if decode(raw, &scopes) != nil || len(scopes) > 128 {
		t.Fatal("invalid real scope inventory")
	}
	snapshots := map[string]any{}
	for _, scope := range scopes {
		kind := ""
		if scope.Name == "roles" {
			kind = "default-client-scopes"
		}
		if scope.Name == "email" {
			kind = "optional-client-scopes"
		}
		if kind == "" {
			continue
		}
		if !identifier.MatchString(scope.ID) {
			t.Fatal("invalid real scope identity")
		}
		var before any
		if decode(request(http.MethodGet, "/client-scopes/"+scope.ID, nil, http.StatusOK), &before) != nil {
			t.Fatal("invalid shared scope")
		}
		snapshots[scope.ID] = before
		request(http.MethodPut, "/clients/"+owner.ID+"/"+kind+"/"+scope.ID, nil, http.StatusNoContent)
	}
	if len(snapshots) != 2 {
		t.Fatal("real shared scope fixture is missing")
	}
	foreign, err := c.FindClient(ctx, "foreign-service")
	if err != nil {
		t.Fatal(err)
	}
	extra, err := c.getRole(ctx, foreign.ID, "foreign-view")
	if err != nil {
		t.Fatal(err)
	}
	path := "/clients/" + owner.ID + "/scope-mappings"
	if err = c.writeRoles(ctx, http.MethodPost, path+"/clients/"+foreign.ID, []RoleRepresentation{extra}); err != nil {
		t.Fatal(err)
	}
	if err = c.InspectClientScopes(ctx, owner, p); !errors.Is(err, ErrScopePolicy) {
		t.Fatal("real excess scopes accepted", err)
	}
	// Client management does not authorize realm-role scope changes in the
	// pinned provider. Prove denial without adding that power to the operator.
	if err = c.ReconcileClientScopes(ctx, owner, p); err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatal("realm-role scope permission was not enforced", err)
	}
	disabled, e := c.InspectClient(ctx, owner)
	if e != nil || disabled.Enabled {
		t.Fatal("denied scope change enabled client", e)
	}
	clientOnly := RolePolicy{Clients: p.Clients}
	if err = c.ReconcileClientScopes(ctx, owner, clientOnly); err != nil {
		t.Fatal("client-only scope policy required realm management", err)
	}
	for i := 0; i < 2; i++ {
		if err = scopeAdmin.ReconcileClientScopes(ctx, owner, p); err != nil {
			t.Fatal("real scope reconciliation failed", err)
		}
	}
	if err = c.InspectClientScopes(ctx, owner, p); err != nil {
		t.Fatal("real scope inspection failed", err)
	}
	var current struct {
		Realm   []RoleRepresentation `json:"realmMappings"`
		Clients map[string]struct {
			ID    string               `json:"id"`
			Roles []RoleRepresentation `json:"mappings"`
		} `json:"clientMappings"`
	}
	if json.Unmarshal(request(http.MethodGet, path, nil, http.StatusOK), &current) != nil || len(current.Realm) != 1 || current.Realm[0].Name != p.Realm[0] || len(current.Clients) != 1 || current.Clients[target.ClientID].ID != target.ID || len(current.Clients[target.ClientID].Roles) != 1 || current.Clients[target.ClientID].Roles[0].Name != p.Clients[0].Names[0] {
		t.Fatal("real scope state differs")
	}
	for _, kind := range []string{"default-client-scopes", "optional-client-scopes"} {
		var remaining []assignedScope
		if decode(request(http.MethodGet, "/clients/"+owner.ID+"/"+kind, nil, http.StatusOK), &remaining) != nil || remaining == nil || len(remaining) != 0 {
			t.Fatal("real shared scope assignment remains")
		}
	}
	for id, before := range snapshots {
		var after any
		if decode(request(http.MethodGet, "/client-scopes/"+id, nil, http.StatusOK), &after) != nil || !reflect.DeepEqual(before, after) {
			t.Fatal("shared scope definition changed")
		}
	}
	value, err := c.InspectClient(ctx, owner)
	if err != nil || value.Enabled {
		t.Fatal("scope reconciliation enabled the client", err)
	}
	if err = scopeAdmin.ReconcileClientScopes(ctx, owner, RolePolicy{}); err != nil {
		t.Fatal("real empty scope policy failed", err)
	}
	if err = c.InspectClientScopes(ctx, owner, RolePolicy{}); err != nil {
		t.Fatal(err)
	}
	if err = scopeAdmin.ReconcileClientScopes(ctx, owner, p); err != nil {
		t.Fatal("real scope restoration failed", err)
	}
	t.Log("Real scope checks: shared scopes detached without definition changes; exact leaf roles; repeated reconciliation; empty policy; client remains disabled")
}
