package keycloak

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

func testLiveServiceAccountLifecycle(t *testing.T, c *Client, ctx context.Context, target ClientBinding) {
	t.Helper()
	for _, legacy := range []bool{false, true} {
		_, f := newAccountLifecycleFixture(t, legacy)
		identity := f.provider.identity
		identity.ClientID = "journal-account-created"
		if legacy {
			identity.ClientID = "journal-account-legacy"
		}
		f.key.ResourceID = identity.ClientID
		f.provider.identity = identity
		base := ServiceAccountPolicy{DisplayName: "Journal batch account", AccessTokenLifetimeSeconds: 300}
		expectedSubject := ""
		expectedProviderID := ""
		if legacy {
			value, err := serviceAccountConfiguration(identity.binding("journal-account-legacy-id"), base)
			if err != nil {
				t.Fatal(err)
			}
			value.Enabled = true
			for old, next := range identity.LegacyRenames {
				delete(value.Attributes, next)
				value.Attributes[old] = identity.LegacyAttributes[old]
			}
			body, _ := json.Marshal(value)
			response, err := c.admin(ctx, http.MethodPost, "/clients", body)
			if err != nil || response.StatusCode != http.StatusCreated {
				t.Fatal("legacy account creation failed", err)
			}
			user, err := c.ResolveServiceAccountUser(ctx, ClientBinding{ID: value.ID, ClientID: identity.ClientID, Attributes: identity.LegacyAttributes})
			if err != nil {
				t.Fatal("legacy subject resolution failed", err)
			}
			expectedSubject = user.ID
		}
		policy := func(ClientBinding) (ServiceAccountLifecyclePolicy, error) {
			roles := RolePolicy{Clients: []ClientRoleGrant{{Client: target, Names: []string{"read"}}}}
			return ServiceAccountLifecyclePolicy{Client: base, ExpectedSubject: expectedSubject, ExpectedProviderID: expectedProviderID, Roles: roles, Scopes: roles, Claims: TokenClaimsPolicy{AudienceClients: []ClientBinding{target}, ClientRoles: []ClientRoleClaim{{Client: target, Claim: "catalog.roles"}}}}, nil
		}
		restart := func() *ServiceAccountClientLifecycle {
			t.Helper()
			l, err := NewServiceAccountClientLifecycle(c, f.restart().journal, identity)
			if err != nil {
				t.Fatal(err)
			}
			return l
		}
		// The subject save commits, but its acknowledgement does not arrive.
		f.failSave = 3
		if legacy {
			f.failSave = 4
		}
		f.lostSave = true
		if _, err := restart().Reconcile(ctx, policy); err == nil {
			t.Fatal("lost account journal acknowledgement was accepted")
		}
		saved := f.read()
		value, err := c.InspectClient(ctx, saved.Binding)
		if err != nil || value.Enabled || saved.Subject == "" {
			t.Fatal("unconfirmed subject save enabled account", err)
		}
		if legacy && saved.Subject != expectedSubject {
			t.Fatal("migration replaced the saved subject")
		}
		f.failSave = 0
		expectedProviderID = saved.Binding.ID
		account, err := restart().Reconcile(ctx, policy)
		if err != nil {
			t.Fatal("service-account lifecycle recovery failed", err)
		}
		desired, _ := policy(account.Client)
		if err = c.InspectServiceAccountAccess(ctx, account.Client, desired.access(account.Subject)); err != nil {
			t.Fatal("service-account lifecycle policy is incomplete", err)
		}
		version := f.record.Version
		if _, err = restart().Reconcile(ctx, policy); err != nil || version != f.record.Version {
			t.Fatal("converged account changed its journal", err)
		}

		disabled := func(b ClientBinding) (ServiceAccountLifecyclePolicy, error) {
			p, err := policy(b)
			p.Disabled = true
			return p, err
		}
		if _, err = restart().Reconcile(ctx, disabled); err != nil {
			t.Fatal("disabled account reconciliation failed", err)
		}
		value, err = c.InspectClient(ctx, account.Client)
		if err != nil || value.Enabled || version != f.record.Version {
			t.Fatal("disabled account changed its identity or stayed enabled", err)
		}
		if _, err = restart().Reconcile(ctx, policy); err != nil {
			t.Fatal("account resume failed", err)
		}
		if err = restart().CloseExisting(ctx, account.Client.ID); err != nil {
			t.Fatal("service-account lifecycle cleanup failed", err)
		}
		if _, err = c.GetClient(ctx, account.Client.ID); !errors.Is(err, ErrNotFound) {
			t.Fatal("closed account remains", err)
		}
		// Reproduce an earlier create that arrives after the first cleanup.
		if _, err = c.CreateDisabledServiceAccount(ctx, account.Client, base); err != nil {
			t.Fatal("late create fixture failed", err)
		}
		if err = restart().CloseExisting(ctx, account.Client.ID); err != nil {
			t.Fatal("retained account cleanup failed", err)
		}
		if _, err = c.GetClient(ctx, account.Client.ID); !errors.Is(err, ErrNotFound) {
			t.Fatal("late account remains", err)
		}
		if _, err = restart().Reconcile(ctx, policy); !errors.Is(err, ErrClientClosed) {
			t.Fatal("closed account was reopened", err)
		}
	}
	t.Log("Real service-account lifecycle passed creation, legacy migration, saved subject, lost journal acknowledgement, restart, signed token policy, disabled repair, resume, and late-create cleanup")
}
