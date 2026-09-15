package keycloak

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

type migrationFixture struct {
	record map[string]any
	fault  string
	writes int
}

func migrationPlan() ClientOwnershipMigration {
	return ClientOwnershipMigration{Prior: ClientBinding{ID: "saved-id", ClientID: "legacy-worker", Attributes: map[string]string{"legacy.product": "catalog", "legacy.object": "object-1"}}, Renames: map[string]string{"legacy.product": "stego.owner.product", "legacy.object": "stego.owner.object"}}
}
func newMigrationFixture(t *testing.T) (*Client, *migrationFixture) {
	t.Helper()
	f := &migrationFixture{record: map[string]any{"id": "saved-id", "clientId": "legacy-worker", "enabled": false, "publicClient": false, "protocol": "openid-connect", "clientAuthenticatorType": "client-secret", "secret": "private-existing-secret", "webOrigins": []string{}, "attributes": map[string]string{"legacy.product": "catalog", "legacy.object": "object-1", "unrelated": "retain"}, "futureField": map[string]any{"keep": true}}}
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if authRequest(w, r) {
			return
		}
		switch r.URL.Path {
		case "/admin/realms/tenant/client-scopes":
			_ = json.NewEncoder(w).Encode([]map[string]string{{"id": "proof", "name": "proof"}})
			return
		case "/admin/realms/tenant/client-scopes/proof":
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "proof", "name": "proof"})
			return
		case "/admin/realms/tenant/clients":
			if r.URL.Query().Get("clientId") != "legacy-worker" || r.URL.Query().Get("max") != "2" || r.URL.Query().Get("search") != "false" {
				t.Error("unbounded legacy discovery")
			}
			_ = json.NewEncoder(w).Encode([]any{f.record})
			return
		case "/admin/realms/tenant/clients/saved-id":
		default:
			t.Error("unexpected migration path")
			w.WriteHeader(500)
			return
		}
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(f.record)
			return
		}
		if r.Method != http.MethodPut {
			t.Error("unexpected migration method")
			w.WriteHeader(500)
			return
		}
		f.writes++
		var body map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&body) != nil || len(body) != 3 || string(body["enabled"]) != "false" || string(body["webOrigins"]) != "[]" {
			t.Error("migration changed non-metadata fields")
			w.WriteHeader(400)
			return
		}
		var patch map[string]string
		_ = json.Unmarshal(body["attributes"], &patch)
		attributes := f.record["attributes"].(map[string]string)
		if f.fault == "partial write" {
			delete(attributes, "legacy.product")
			attributes["stego.owner.product"] = "catalog"
			w.WriteHeader(503)
			return
		}
		for key, value := range patch {
			if value == "" {
				if f.fault != "ignored removal" {
					delete(attributes, key)
				}
			} else {
				attributes[key] = value
			}
		}
		if f.fault == "changed secret" {
			f.record["secret"] = "private-changed-secret"
		}
		if f.fault == "changed future field" {
			f.record["futureField"] = map[string]any{"keep": false}
		}
		if f.fault == "changed owner" {
			attributes["stego.owner.object"] = "another-object"
		}
		w.WriteHeader(204)
	})
	return c, f
}

func preparedMigration(t *testing.T, c *Client) ClientOwnershipMigration {
	t.Helper()
	plan := migrationPlan()
	prepared, err := c.PrepareClientOwnershipMigration(context.Background(), plan.Prior, plan.Renames)
	if err != nil {
		t.Fatal("migration preparation failed", err)
	}
	checkpoint, err := prepared.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := RestoreOwnershipMigration(checkpoint.Reveal())
	if err != nil {
		t.Fatal(err)
	}
	return restored
}

func TestOwnershipDiscoveryAndMigration(t *testing.T) {
	c, f := newMigrationFixture(t)
	plan := migrationPlan()
	found, err := c.DiscoverOwnedClient(context.Background(), plan.Prior.ClientID, plan.Prior.Attributes)
	if err != nil || !reflect.DeepEqual(found, plan.Prior) || f.writes != 0 {
		t.Fatal("legacy discovery failed", err)
	}
	found.Attributes["legacy.object"] = "changed copy"
	if plan.Prior.Attributes["legacy.object"] != "object-1" {
		t.Fatal("discovery shared caller ownership state")
	}
	plan = preparedMigration(t, c)
	next, err := c.MigrateClientOwnership(context.Background(), plan)
	expected, _ := plan.NextBinding()
	if err != nil || !reflect.DeepEqual(next, expected) || f.writes != 1 {
		t.Fatal("migration failed", err)
	}
	attributes := f.record["attributes"].(map[string]string)
	if attributes["unrelated"] != "retain" || f.record["secret"] != "private-existing-secret" {
		t.Fatal("migration lost existing state")
	}
	if _, err = c.MigrateClientOwnership(context.Background(), plan); err != nil || f.writes != 1 {
		t.Fatal("completed migration was rewritten", err)
	}
	f.record["enabled"] = true
	if _, err = c.MigrateClientOwnership(context.Background(), plan); err == nil || f.writes != 1 {
		t.Fatal("changed completed record was accepted")
	}
}

func TestOwnershipMigrationFailures(t *testing.T) {
	for _, fault := range []string{"enabled", "conflicting legacy value", "conflicting target value", "other owner", "missing pair", "missing secret", "ignored removal", "partial write", "changed secret", "changed future field", "changed owner"} {
		t.Run(fault, func(t *testing.T) {
			c, f := newMigrationFixture(t)
			plan := preparedMigration(t, c)
			f.fault = fault
			attrs := f.record["attributes"].(map[string]string)
			noWrite := true
			switch fault {
			case "enabled":
				f.record["enabled"] = true
			case "conflicting legacy value":
				attrs["legacy.object"] = "another"
			case "conflicting target value":
				attrs["stego.owner.object"] = "another"
			case "other owner":
				attrs["stego.owner.unrelated"] = "other"
			case "missing pair":
				delete(attrs, "legacy.object")
			case "missing secret":
				delete(f.record, "secret")
			default:
				noWrite = false
			}
			_, err := c.MigrateClientOwnership(context.Background(), plan)
			if err == nil {
				t.Fatal("unconfirmed migration accepted")
			}
			if noWrite && f.writes != 0 {
				t.Fatal("unsafe migration changed state")
			}
			if !noWrite && f.writes != 1 {
				t.Fatal("migration write was replayed")
			}
			if strings.Contains(err.Error(), "private-") {
				t.Fatal("migration error exposed credentials")
			}
			if fault == "changed secret" || fault == "changed future field" {
				f.fault = ""
				if _, err = c.MigrateClientOwnership(context.Background(), plan); err == nil || f.writes != 1 {
					t.Fatal("retry hid a change to protected state")
				}
			}
			if fault == "ignored removal" || fault == "partial write" {
				f.fault = ""
				if _, err = c.MigrateClientOwnership(context.Background(), plan); err != nil || f.writes != 2 {
					t.Fatal("saved migration plan could not recover", err)
				}
			}
		})
	}
}

func TestOwnershipDiscoveryRejectsForeignRecords(t *testing.T) {
	c, f := newMigrationFixture(t)
	plan := migrationPlan()
	f.record["attributes"].(map[string]string)["legacy.object"] = "foreign"
	if _, err := c.DiscoverOwnedClient(context.Background(), plan.Prior.ClientID, plan.Prior.Attributes); !errors.Is(err, ErrOwnership) || f.writes != 0 {
		t.Fatal("foreign discovery was accepted", err)
	}
}

func TestOwnershipMigrationDoesNotChangeValues(t *testing.T) {
	for _, renames := range []map[string]string{
		nil,
		{"legacy.product": "stego.owner.product"},
		{"legacy.product": "pkce.code.challenge.method", "legacy.object": "stego.owner.object"},
		{"legacy.product": "stego.owner.product", "legacy.object": "stego.owner.product"},
	} {
		plan := migrationPlan()
		plan.Renames = renames
		if _, err := plan.NextBinding(); err == nil {
			t.Fatal("invalid ownership rename accepted")
		}
	}
}

func TestOwnershipMigrationCheckpoint(t *testing.T) {
	c, f := newMigrationFixture(t)
	plan := migrationPlan()
	if _, err := c.MigrateClientOwnership(context.Background(), plan); err == nil || f.writes != 0 {
		t.Fatal("unprepared migration accepted")
	}
	if _, err := plan.Checkpoint(); err == nil {
		t.Fatal("unprepared checkpoint exported")
	}
	prepared := preparedMigration(t, c)
	secret, err := prepared.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(secret.Reveal(), "private-existing-secret") {
		t.Fatal("checkpoint contains plaintext credential")
	}
	for _, value := range []any{prepared, secret} {
		if strings.Contains(fmt.Sprintf("%v %#v", value, value), prepared.fingerprint) {
			t.Fatal("format exposed protected checkpoint")
		}
		if _, err := json.Marshal(value); err == nil {
			t.Fatal("implicit checkpoint serialization accepted")
		}
	}
	for _, raw := range []string{"", `{}`, strings.Repeat("x", 32769), strings.Replace(secret.Reveal(), `"version":1`, `"version":2`, 1), strings.Replace(secret.Reveal(), prepared.fingerprint, "bad", 1)} {
		if _, err := RestoreOwnershipMigration(raw); err == nil {
			t.Fatal("invalid checkpoint accepted")
		}
	}
	f.record["attributes"].(map[string]string)["stego.owner.product"] = "catalog"
	if _, err := c.PrepareClientOwnershipMigration(context.Background(), plan.Prior, plan.Renames); err == nil || f.writes != 0 {
		t.Fatal("partially applied migration accepted as a new baseline")
	}
}

func TestOwnershipMigrationHashPreservesLargeNumbers(t *testing.T) {
	plan := migrationPlan()
	before, err := migrationRecordHash([]byte(`{"attributes":{},"future":9007199254740992}`), plan)
	if err != nil {
		t.Fatal(err)
	}
	after, err := migrationRecordHash([]byte(`{"attributes":{},"future":9007199254740993}`), plan)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("record hash lost numeric precision")
	}
}
