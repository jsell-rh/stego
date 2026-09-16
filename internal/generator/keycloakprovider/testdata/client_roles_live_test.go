package keycloak

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A client-only policy must work without view-realm or manage-realm. The
// provider ID differs from the public client ID, and ownership uses legacy keys.
func testLiveClientOnlyRoles(t *testing.T, ctx context.Context, options Options) {
	t.Helper()
	options.ClientID = "role-operator"
	options.SecretFile = filepath.Join(t.TempDir(), "role-secret")
	if err := os.WriteFile(options.SecretFile, []byte("provider-test-role-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	target := ClientBinding{ID: "client-only-target-id", ClientID: "client-only-target", Attributes: map[string]string{"application.object": "object-4"}}
	worker := ClientBinding{ID: "client-only-worker-id", ClientID: "client-only-worker", Attributes: map[string]string{"application.worker": "worker-4"}}
	create := func(b ClientBinding, serviceAccount bool) {
		t.Helper()
		payload, _ := json.Marshal(map[string]any{"id": b.ID, "clientId": b.ClientID, "enabled": false, "protocol": "openid-connect", "publicClient": false, "clientAuthenticatorType": "client-secret", "serviceAccountsEnabled": serviceAccount, "standardFlowEnabled": false, "implicitFlowEnabled": false, "directAccessGrantsEnabled": false, "authorizationServicesEnabled": false, "fullScopeAllowed": false, "redirectUris": []string{}, "webOrigins": []string{}, "defaultClientScopes": []string{}, "optionalClientScopes": []string{}, "attributes": b.Attributes})
		response, err := c.admin(ctx, http.MethodPost, "/clients", payload)
		if err != nil || response.StatusCode != http.StatusCreated {
			t.Fatal("client-only fixture creation failed", err, response.StatusCode)
		}
	}
	create(target, false)
	create(worker, true)
	if _, err := c.EnsureClientRoles(ctx, target, []string{"read", "write"}); err != nil {
		t.Fatal("client-only role definitions failed", err)
	}
	subject, err := c.ResolveServiceAccountUser(ctx, worker)
	if err != nil {
		t.Fatal("client-only subject resolution failed", err)
	}
	policy := ServiceAccountRolePolicy{Clients: []ClientRoleGrant{{Client: target, Names: []string{"read", "write"}}}}
	started := time.Now()
	if err := c.ReconcileServiceAccountRoles(ctx, worker, subject.ID, policy); err != nil {
		t.Fatal("client-only role reconciliation failed", err)
	}
	if err := c.InspectServiceAccountRoles(ctx, worker, subject.ID, policy); err != nil {
		t.Fatal("disabled client-only role inspection failed", err)
	}
	t.Log("Client-only role reconciliation and inspection duration", time.Since(started))
	response, err := c.admin(ctx, http.MethodPut, "/clients/"+worker.ID, []byte(`{"enabled":true}`))
	if err != nil || response.StatusCode != http.StatusNoContent {
		t.Fatal("client-only fixture enablement failed", err)
	}
	if err := c.InspectServiceAccountRoles(ctx, worker, subject.ID, policy); err != nil {
		t.Fatal("enabled client-only role inspection failed", err)
	}
	if err := c.DeleteClient(ctx, worker); err != nil {
		t.Fatal("client-only worker cleanup failed", err)
	}
	if err := c.DeleteClient(ctx, target); err != nil {
		t.Fatal("client-only target cleanup failed", err)
	}
	t.Log("Client-only roles passed with restricted permissions, distinct IDs, legacy ownership, and both client states")
}
