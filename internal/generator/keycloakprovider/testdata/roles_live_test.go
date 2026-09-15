package keycloak

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"testing"
)

func testLiveRolePolicies(t *testing.T, c, scopeAdmin *Client, ctx context.Context) {
	t.Helper()
	request := func(method, path string, body []byte, status int) []byte {
		t.Helper()
		response, err := c.admin(ctx, method, path, body)
		if err != nil || response.StatusCode != status {
			t.Fatal("real role fixture request failed", method, status, err)
		}
		return response.Body
	}
	groupsRaw := request(http.MethodGet, "/groups?search=outside&exact=true&briefRepresentation=true&first=0&max=2", nil, http.StatusOK)
	var groups []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if decode(groupsRaw, &groups) != nil || len(groups) != 1 || groups[0].Name != "outside" || !identifier.MatchString(groups[0].ID) {
		t.Fatal("real role fixture group is missing")
	}
	groupID := groups[0].ID
	foreign, err := c.FindClient(ctx, "foreign-service")
	if err != nil {
		t.Fatal(err)
	}
	foreignRole, err := c.getRole(ctx, foreign.ID, "foreign-view")
	if err != nil {
		t.Fatal(err)
	}
	realmExtra, err := c.getRole(ctx, "", "shared-global")
	if err != nil {
		t.Fatal(err)
	}
	// Compare identities without relying on the reconciliation difference helper.
	identities := func(roles []RoleRepresentation) string {
		keys := make([]string, 0, len(roles))
		for _, r := range roles {
			keys = append(keys, r.ID+":"+r.ContainerID+":"+r.Name)
		}
		sort.Strings(keys)
		return strings.Join(keys, "\n")
	}
	for _, application := range []struct{ name, key, value, user, read, write string }{
		{"catalog", "product.owner", "object-1", "shared-catalog", "catalog-read", "catalog-write"},
		{"batch-worker", "pipeline.job", "run-2", "shared-pipeline", "pipeline-execute", "pipeline-audit"},
	} {
		targetClient, err := c.FindClient(ctx, application.name)
		if err != nil {
			t.Fatal(err)
		}
		target := ClientBinding{ID: targetClient.ID, ClientID: application.name, Attributes: map[string]string{application.key: application.value}}
		if err = c.DisableClient(ctx, target); err != nil {
			t.Fatal(err)
		}
		roles, err := c.EnsureClientRoles(ctx, target, []string{application.read, application.write})
		if err != nil || len(roles) != 2 {
			t.Fatal("real client role creation failed", err)
		}
		if _, err = c.EnsureClientRoles(ctx, target, []string{application.read, application.write}); err != nil {
			t.Fatal("real client roles did not converge", err)
		}
		userPath := "/users/" + application.user + "/role-mappings"
		if err = c.writeRoles(ctx, http.MethodPost, userPath+"/clients/"+target.ID, roles[1:]); err != nil {
			t.Fatal(err)
		}
		before, err := c.readRoleSet(ctx, userPath)
		if err != nil {
			t.Fatal(err)
		}
		beforeGroups, err := c.serviceAccountGroups(ctx, application.user)
		if err != nil {
			t.Fatal(err)
		}
		if err = c.ReconcileUserClientRoles(ctx, target, application.user, []string{application.read}); err != nil {
			t.Fatal("real scoped user role update failed", err)
		}
		if err = c.ReconcileUserClientRoles(ctx, target, application.user, []string{application.read}); err != nil {
			t.Fatal("real scoped user roles did not converge", err)
		}
		after, err := c.readRoleSet(ctx, userPath)
		if err != nil {
			t.Fatal(err)
		}
		afterGroups, err := c.serviceAccountGroups(ctx, application.user)
		if err != nil {
			t.Fatal(err)
		}
		if identities(before.Realm) != identities(after.Realm) || identities(before.Clients[foreign.ID]) != identities(after.Clients[foreign.ID]) || strings.Join(beforeGroups, ",") != strings.Join(afterGroups, ",") || len(after.Clients[target.ID]) != 1 || after.Clients[target.ID][0].Name != application.read {
			t.Fatal("real scoped update changed unrelated user access")
		}
		// The shared-user operation must not remove a group or claim that it has
		// removed excess roles inherited from that group.
		groupPath := "/groups/" + groupID + "/role-mappings/clients/" + target.ID
		if err = c.writeRoles(ctx, http.MethodPost, groupPath, roles[1:]); err != nil {
			t.Fatal(err)
		}
		if err = c.ReconcileUserClientRoles(ctx, target, application.user, []string{application.read}); !errors.Is(err, ErrRolePolicy) {
			t.Fatal("real inherited excess role was accepted", err)
		}
		retained, err := c.serviceAccountGroups(ctx, application.user)
		if err != nil || strings.Join(retained, ",") != strings.Join(beforeGroups, ",") {
			t.Fatal("scoped operation removed a shared group", err)
		}
		if err = c.writeRoles(ctx, http.MethodDelete, groupPath, roles[1:]); err != nil {
			t.Fatal(err)
		}

		owner := ClientBinding{ID: "role-" + application.user, ClientID: "role-" + application.user, Attributes: map[string]string{"stego.owner.role-test": application.user}}
		if _, err = c.CreateDisabledServiceAccount(ctx, owner, ServiceAccountPolicy{DisplayName: "Role worker", AccessTokenLifetimeSeconds: 300}); err != nil {
			t.Fatal(err)
		}
		subject, err := c.ResolveServiceAccountUser(ctx, owner)
		if err != nil {
			t.Fatal(err)
		}
		policy := ServiceAccountRolePolicy{Realm: []string{"worker-base"}, Clients: []ClientRoleGrant{{Client: target, Names: []string{application.read}}}}
		if err = c.ReconcileServiceAccountRoles(ctx, owner, application.user, policy); !errors.Is(err, ErrOwnership) {
			t.Fatal("full role operation accepted another user", err)
		}
		servicePath := "/users/" + subject.ID + "/role-mappings"
		request(http.MethodPut, "/users/"+subject.ID+"/groups/"+groupID, nil, http.StatusNoContent)
		if err = c.writeRoles(ctx, http.MethodPost, servicePath+"/realm", []RoleRepresentation{realmExtra}); err != nil {
			t.Fatal(err)
		}
		if err = c.writeRoles(ctx, http.MethodPost, servicePath+"/clients/"+foreign.ID, []RoleRepresentation{foreignRole}); err != nil {
			t.Fatal(err)
		}
		if err = c.writeRoles(ctx, http.MethodPost, servicePath+"/clients/"+target.ID, roles[1:]); err != nil {
			t.Fatal(err)
		}
		if err = c.ReconcileServiceAccountRoles(ctx, owner, subject.ID, policy); err != nil {
			t.Fatal("real full service-account role update failed", err)
		}
		if err = c.ReconcileServiceAccountRoles(ctx, owner, subject.ID, policy); err != nil {
			t.Fatal("real full service-account roles did not converge", err)
		}
		if err = c.InspectServiceAccountRoles(ctx, owner, subject.ID, policy); err != nil {
			t.Fatal("real service-account role inspection failed", err)
		}
		request(http.MethodPut, "/users/"+subject.ID+"/groups/"+groupID, nil, http.StatusNoContent)
		if err = c.InspectServiceAccountRoles(ctx, owner, subject.ID, policy); !errors.Is(err, ErrRolePolicy) {
			t.Fatal("real role inspection missed a group", err)
		}
		if err = c.ReconcileServiceAccountRoles(ctx, owner, subject.ID, policy); err != nil {
			t.Fatal("real group repair failed", err)
		}
		raw := request(http.MethodGet, servicePath, nil, http.StatusOK)
		var final struct {
			Realm []struct {
				Name string `json:"name"`
			} `json:"realmMappings"`
			Clients map[string]struct {
				ID       string `json:"id"`
				Mappings []struct {
					Name string `json:"name"`
				} `json:"mappings"`
			} `json:"clientMappings"`
		}
		if json.Unmarshal(raw, &final) != nil || len(final.Realm) != 1 || final.Realm[0].Name != "worker-base" || len(final.Clients) != 1 || final.Clients[application.name].ID != target.ID || len(final.Clients[application.name].Mappings) != 1 || final.Clients[application.name].Mappings[0].Name != application.read {
			t.Fatal("real service-account role set differs")
		}
		remaining, err := c.serviceAccountGroups(ctx, subject.ID)
		if err != nil || len(remaining) != 0 {
			t.Fatal("real service-account retained group access", err)
		}
		stillShared, err := c.readRoleSet(ctx, userPath)
		if err != nil || identities(stillShared.Realm) != identities(before.Realm) || identities(stillShared.Clients[foreign.ID]) != identities(before.Clients[foreign.ID]) {
			t.Fatal("full service-account update changed the shared user", err)
		}
		testLiveScopePolicy(t, c, scopeAdmin, ctx, owner, target, policy)
		testLiveMapperPolicy(t, c, ctx, owner, target, subject.ID, policy, groupID)
		if err = c.DeleteClient(ctx, owner); err != nil {
			t.Fatal(err)
		}
	}
	t.Log("Real role checks: two policies; shared users retain other roles and groups; inherited excess access is denied; owned service accounts lose excess realm, client, and group access; wrong subjects are denied")
}
