package keycloak

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
)

func testLiveOwnershipMigration(t *testing.T, c *Client, ctx context.Context, b ClientBinding) {
	t.Helper()
	value, raw, err := c.boundClient(ctx, b)
	if err != nil || !explicitFlag(raw, "enabled", false) {
		t.Fatal("migration fixture is not disabled", err)
	}
	var credential Secret
	if !value.PublicClient {
		credential, err = c.GetClientSecret(ctx, b)
		if err != nil {
			t.Fatal(err)
		}
	}
	mappers, err := c.readMappers(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	migration := ClientOwnershipMigration{Prior: ClientBinding{ID: b.ID, ClientID: b.ClientID, Attributes: map[string]string{}}, Renames: map[string]string{}}
	patch := map[string]string{}
	for key, expected := range b.Attributes {
		legacy := "legacy." + key
		migration.Prior.Attributes[legacy] = expected
		migration.Renames[legacy] = key
		patch[key] = ""
		patch[legacy] = expected
	}
	body, _ := json.Marshal(map[string]any{"enabled": false, "webOrigins": value.WebOrigins, "attributes": patch})
	response, err := c.admin(ctx, http.MethodPut, "/clients/"+b.ID, body)
	if err != nil || response.StatusCode != 204 {
		t.Fatal("legacy ownership fixture failed", err)
	}
	// The fixture already retains this stable ID. A real legacy caller must save
	// the discovered binding before it sends a migration request.
	found, err := c.DiscoverOwnedClient(ctx, b.ClientID, migration.Prior.Attributes)
	if err != nil || !reflect.DeepEqual(found, migration.Prior) {
		t.Fatal("real ownership discovery differs", err)
	}
	migration, err = c.PrepareClientOwnershipMigration(ctx, found, migration.Renames)
	if err != nil {
		t.Fatal("real migration preparation failed", err)
	}
	checkpoint, err := migration.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	// This disposable fixture holds the protected checkpoint in memory. The
	// application must commit it to encrypted storage before the first write.
	migration, err = RestoreOwnershipMigration(checkpoint.Reveal())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		next, err := c.MigrateClientOwnership(ctx, migration)
		if err != nil || !reflect.DeepEqual(next, b) {
			t.Fatal("real ownership migration failed", err)
		}
	}
	after, err := c.readMappers(ctx, b)
	if err != nil || !reflect.DeepEqual(mappers, after) {
		t.Fatal("migration changed mapper state", err)
	}
	if !value.PublicClient {
		after, err := c.GetClientSecret(ctx, b)
		if err != nil || after.Reveal() != credential.Reveal() {
			t.Fatal("migration changed the client credential", err)
		}
	}
	t.Log("Real ownership migration passed; stable IDs, credential, and mapper state retained")
}
