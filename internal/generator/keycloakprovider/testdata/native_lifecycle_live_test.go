package keycloak

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

func testLiveNativeLifecycle(t *testing.T, client *Client, ctx context.Context) {
	t.Helper()
	for _, legacy := range []bool{false, true} {
		_, fixture := newLifecycleFixture(t, legacy)
		identity := fixture.provider.identity
		identity.ClientID = "journal-native-created"
		if legacy {
			identity.ClientID = "journal-native-legacy"
		}
		fixture.key.ResourceID = identity.ClientID
		fixture.provider.identity = identity
		if legacy {
			_, base := nativeInputs()
			value, err := nativeClientConfiguration(identity.binding("journal-legacy-provider-id"), base)
			if err != nil {
				t.Fatal(err)
			}
			value.Enabled = true
			for old, next := range identity.LegacyRenames {
				delete(value.Attributes, next)
				value.Attributes[old] = identity.LegacyAttributes[old]
			}
			body, _ := json.Marshal(value)
			response, err := client.admin(ctx, http.MethodPost, "/clients", body)
			if err != nil || response.StatusCode != http.StatusCreated {
				t.Fatal("legacy native lifecycle fixture failed", err)
			}
		}
		restart := func() *NativeClientLifecycle {
			t.Helper()
			journal := fixture.restart().journal
			lifecycle, err := NewNativeClientLifecycle(client, journal, identity)
			if err != nil {
				t.Fatal(err)
			}
			return lifecycle
		}
		// Lose the acknowledgement after the new binding commits. The provider must
		// remain disabled until another journal reads the committed boundary.
		fixture.failSave = 2
		if legacy {
			fixture.failSave = 3
		}
		fixture.lostSave = true
		if _, err := restart().Reconcile(ctx, lifecyclePolicy); err == nil {
			t.Fatal("lost native journal acknowledgement was accepted")
		}
		saved := fixture.read()
		value, err := client.InspectClient(ctx, saved.Binding)
		if err != nil || value.Enabled {
			t.Fatal("unconfirmed journal save enabled native login", err)
		}
		fixture.failSave = 0
		binding, err := restart().Reconcile(ctx, lifecyclePolicy)
		if err != nil {
			t.Fatal("native lifecycle recovery failed", err)
		}
		policy, err := lifecyclePolicy(binding)
		if err != nil {
			t.Fatal(err)
		}
		if err = client.InspectNativeClientAccess(ctx, binding, policy); err != nil {
			t.Fatal("native lifecycle policy is incomplete", err)
		}
		version := fixture.record.Version
		if _, err = restart().Reconcile(ctx, lifecyclePolicy); err != nil || fixture.record.Version != version {
			t.Fatal("converged native lifecycle changed its journal", err)
		}
		if err = restart().Close(ctx); err != nil {
			t.Fatal("native lifecycle cleanup failed", err)
		}
		if _, err = client.GetClient(ctx, binding.ID); !errors.Is(err, ErrNotFound) {
			t.Fatal("closed native client remains", err)
		}
		if err = restart().Close(ctx); err != nil {
			t.Fatal("retained native cleanup failed", err)
		}
		if _, err = restart().Reconcile(ctx, lifecyclePolicy); !errors.Is(err, ErrClientClosed) {
			t.Fatal("closed native client was reopened", err)
		}
	}
	t.Log("Real native lifecycle passed creation, legacy migration, lost journal acknowledgement, new journal recovery, access policy, and retained cleanup")
}
