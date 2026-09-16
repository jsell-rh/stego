package keycloak

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

type accountLifecycleFixture struct {
	*lifecycleFixture
	account *accountLifecycleProvider
}
type accountLifecycleProvider struct {
	*lifecycleProvider
	subject        string
	subjectCreates int
}

func newAccountLifecycleFixture(t *testing.T, legacy bool) (*ServiceAccountClientLifecycle, *accountLifecycleFixture) {
	_, base := newLifecycleFixture(t, legacy)
	f := &accountLifecycleFixture{lifecycleFixture: base, account: &accountLifecycleProvider{lifecycleProvider: base.provider}}
	if legacy {
		f.account.subject = "legacy-subject"
	}
	return f.restartAccount(), f
}
func (f *accountLifecycleFixture) restartAccount() *ServiceAccountClientLifecycle {
	return &ServiceAccountClientLifecycle{clientLifecycle: &clientLifecycle{provider: f.account, journal: f.restart().journal, identity: f.provider.identity, kind: "service-account"}, account: f.account}
}
func (p *accountLifecycleProvider) CreateDisabledServiceAccount(ctx context.Context, b ClientBinding, _ ServiceAccountPolicy) (ClientRepresentation, error) {
	return p.CreateDisabledNativeClient(ctx, b, NativeClientPolicy{})
}
func (p *accountLifecycleProvider) ResolveServiceAccountUser(_ context.Context, b ClientBinding) (UserRepresentation, error) {
	p.note("subject")
	r := p.fixture.read()
	if r.Kind != "service-account" || r.Phase != "bound" || r.Closed || p.enabled || !reflect.DeepEqual(r.Binding, b) {
		p.fixture.t.Fatal("subject resolution has no disabled saved binding")
	}
	if p.subject == "" {
		p.subject = "new-subject"
		p.subjectCreates++
	}
	if p.fault == "subject result lost" {
		return UserRepresentation{}, errors.New("subject response lost")
	}
	return UserRepresentation{ID: p.subject, Enabled: true, ServiceAccountClientID: b.ClientID}, nil
}
func (p *accountLifecycleProvider) ReconcileServiceAccountAccess(_ context.Context, b ClientBinding, policy ServiceAccountAccessPolicy) error {
	p.note("account access")
	r := p.fixture.read()
	if r.Kind != "service-account" || r.Phase != "bound" || r.Closed || r.Subject == "" || r.Subject != policy.Subject || !reflect.DeepEqual(r.Binding, b) {
		p.fixture.t.Fatal("access repair has no saved subject and binding")
	}
	if policy.Subject != p.subject {
		p.enabled = false
		return ErrRolePolicy
	}
	p.enabled = true
	return nil
}
func (p *accountLifecycleProvider) ReconcileDisabledServiceAccountAccess(ctx context.Context, b ClientBinding, policy ServiceAccountAccessPolicy) error {
	err := p.ReconcileServiceAccountAccess(ctx, b, policy)
	p.enabled = false
	return err
}
func TestServiceAccountLifecycleDisabledPolicy(t *testing.T) {
	l, f := newAccountLifecycleFixture(t, false)
	if _, err := l.Reconcile(context.Background(), accountLifecyclePolicy); err != nil {
		t.Fatal(err)
	}
	before := f.record.Version
	got, err := f.restartAccount().Reconcile(context.Background(), func(b ClientBinding) (ServiceAccountLifecyclePolicy, error) {
		p, _ := accountLifecyclePolicy(b)
		p.Disabled = true
		return p, nil
	})
	if err != nil || got.Subject == "" || f.provider.enabled || f.record.Version != before {
		t.Fatal("disabled policy changed identity or enabled account", err)
	}
}

func accountLifecyclePolicy(b ClientBinding) (ServiceAccountLifecyclePolicy, error) {
	_, target := roleBindings()
	roles := RolePolicy{Clients: []ClientRoleGrant{{Client: target, Names: []string{"read"}}}}
	return ServiceAccountLifecyclePolicy{Client: ServiceAccountPolicy{DisplayName: "Batch account", AccessTokenLifetimeSeconds: 300}, Roles: roles, Scopes: roles, Claims: TokenClaimsPolicy{AudienceClients: []ClientBinding{target}, ClientRoles: []ClientRoleClaim{{Client: target, Claim: "catalog.roles"}}}}, nil
}
func TestServiceAccountLifecycleRecovery(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		l, f := newAccountLifecycleFixture(t, legacy)
		got, err := l.Reconcile(context.Background(), accountLifecyclePolicy)
		if err != nil {
			t.Fatal(err)
		}
		if got.Subject == "" || got.Subject != f.read().Subject || !f.provider.enabled {
			t.Fatal("subject was not saved before access")
		}
		if legacy && got.Client.ID != "legacy-id" {
			t.Fatal("migration replaced provider identity")
		}
		version := f.record.Version
		f.provider.calls = nil
		again, err := f.restartAccount().Reconcile(context.Background(), accountLifecyclePolicy)
		if err != nil || !reflect.DeepEqual(got, again) || f.record.Version != version || !reflect.DeepEqual(f.provider.calls, []string{"account access"}) {
			t.Fatal("restart changed saved identity", err, f.provider.calls)
		}
		if err = l.Close(context.Background()); err != nil || !f.read().Closed || f.provider.exists {
			t.Fatal("cleanup failed", err)
		}
		f.provider.exists = true // A create accepted before closure can arrive late.
		if err = f.restartAccount().Close(context.Background()); err != nil || f.provider.exists {
			t.Fatal("retained cleanup missed late create", err)
		}
		if _, err = l.Reconcile(context.Background(), accountLifecyclePolicy); !errors.Is(err, ErrClientClosed) {
			t.Fatal("closed account reopened", err)
		}
	}
}
func TestServiceAccountLifecycleSaveBoundaries(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		last := 3
		if legacy {
			last = 4
		}
		for boundary := 1; boundary <= last; boundary++ {
			for _, lost := range []bool{false, true} {
				l, f := newAccountLifecycleFixture(t, legacy)
				f.failSave = boundary
				f.lostSave = lost
				if _, err := l.Reconcile(context.Background(), accountLifecyclePolicy); err == nil {
					t.Fatal("failed save accepted")
				}
				for _, call := range f.provider.calls {
					if call == "account access" {
						t.Fatal("unconfirmed save reached access repair")
					}
				}
				f.failSave = 0
				got, err := f.restartAccount().Reconcile(context.Background(), accountLifecyclePolicy)
				if err != nil || got.Subject == "" || !f.provider.enabled {
					t.Fatal("save boundary recovery failed", legacy, boundary, lost, err)
				}
				if f.account.subjectCreates > 1 {
					t.Fatal("recovery created another subject")
				}
			}
		}
	}
}
func TestServiceAccountLifecycleUncertainEffects(t *testing.T) {
	for _, fault := range []string{"create result lost", "migration result lost", "subject result lost"} {
		l, f := newAccountLifecycleFixture(t, fault == "migration result lost")
		f.provider.fault = fault
		if _, err := l.Reconcile(context.Background(), accountLifecyclePolicy); err == nil {
			t.Fatal("uncertain effect accepted")
		}
		if f.provider.enabled {
			t.Fatal("uncertain effect enabled account")
		}
		f.provider.fault = ""
		if _, err := f.restartAccount().Reconcile(context.Background(), accountLifecyclePolicy); err != nil {
			t.Fatal(err)
		}
		if f.account.subjectCreates > 1 {
			t.Fatal("uncertain response replaced subject")
		}
	}
}
func TestServiceAccountLifecycleRejectsChangedSubject(t *testing.T) {
	l, f := newAccountLifecycleFixture(t, false)
	if _, err := l.Reconcile(context.Background(), accountLifecyclePolicy); err != nil {
		t.Fatal(err)
	}
	f.provider.calls = nil
	if _, err := l.Reconcile(context.Background(), func(b ClientBinding) (ServiceAccountLifecyclePolicy, error) {
		p, _ := accountLifecyclePolicy(b)
		p.ExpectedSubject = "foreign"
		return p, nil
	}); !errors.Is(err, ErrClientLifecycle) {
		t.Fatal("changed saved subject accepted", err)
	}
	if len(f.provider.calls) != 0 {
		t.Fatal("invalid subject reached provider")
	}
	f.account.subject = "replacement"
	if _, err := l.Reconcile(context.Background(), accountLifecyclePolicy); err == nil {
		t.Fatal("provider subject replacement accepted")
	}
	if f.read().Subject == f.account.subject || f.provider.enabled {
		t.Fatal("saved subject replaced or account enabled")
	}
}
func TestServiceAccountLifecycleKindBoundary(t *testing.T) {
	l, f := newAccountLifecycleFixture(t, false)
	if _, err := l.Reconcile(context.Background(), accountLifecyclePolicy); err != nil {
		t.Fatal(err)
	}
	f.provider.calls = nil
	if _, err := f.restart().Reconcile(context.Background(), lifecyclePolicy); !errors.Is(err, ErrClientLifecycle) {
		t.Fatal("native lifecycle accepted service-account state")
	}
	if err := f.restart().Close(context.Background()); !errors.Is(err, ErrClientLifecycle) {
		t.Fatal("native cleanup accepted service-account state")
	}
	if len(f.provider.calls) != 0 {
		t.Fatal("wrong lifecycle kind reached provider")
	}
	native, other := newLifecycleFixture(t, false)
	if _, err := native.Reconcile(context.Background(), lifecyclePolicy); err != nil {
		t.Fatal(err)
	}
	wrong := &accountLifecycleFixture{lifecycleFixture: other, account: &accountLifecycleProvider{lifecycleProvider: other.provider}}
	if _, err := wrong.restartAccount().Reconcile(context.Background(), accountLifecyclePolicy); !errors.Is(err, ErrClientLifecycle) {
		t.Fatal("service-account lifecycle accepted native state")
	}
}

func TestServiceAccountLifecycleRequiresSavedProviderID(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		l, f := newAccountLifecycleFixture(t, legacy)
		policy := func(b ClientBinding) (ServiceAccountLifecyclePolicy, error) {
			p, _ := accountLifecyclePolicy(b)
			p.ExpectedProviderID = "saved-id"
			return p, nil
		}
		if _, err := l.Reconcile(context.Background(), policy); !errors.Is(err, ErrClientLifecycle) {
			t.Fatal("missing or replacement provider ID was accepted", err)
		}
		if f.record.Version != 0 {
			t.Fatal("invalid provider ID changed journal")
		}
		for _, call := range f.provider.calls {
			if call != "find" && call != "discover" {
				t.Fatal("invalid provider ID reached effects", call)
			}
		}
	}
	l, f := newAccountLifecycleFixture(t, true)
	policy := func(b ClientBinding) (ServiceAccountLifecyclePolicy, error) {
		p, _ := accountLifecyclePolicy(b)
		p.ExpectedProviderID = "legacy-id"
		p.ExpectedSubject = "legacy-subject"
		return p, nil
	}
	if _, err := l.Reconcile(context.Background(), policy); err != nil {
		t.Fatal("saved legacy identity did not migrate", err)
	}
	f.provider.calls = nil
	if err := l.CloseExisting(context.Background(), "foreign-id"); !errors.Is(err, ErrClientLifecycle) {
		t.Fatal("cleanup ignored saved provider ID", err)
	}
	if len(f.provider.calls) != 0 {
		t.Fatal("invalid cleanup ID reached provider")
	}
	if err := l.CloseExisting(context.Background(), "legacy-id"); err != nil {
		t.Fatal(err)
	}
}
func TestServiceAccountLifecycleClosesAbsentSavedID(t *testing.T) {
	l, f := newAccountLifecycleFixture(t, true)
	f.provider.exists = false
	if err := l.CloseExisting(context.Background(), "legacy-id"); err != nil {
		t.Fatal(err)
	}
	record := f.read()
	if !record.Closed || record.Binding.ID != "legacy-id" {
		t.Fatal("absent saved identity was not retained")
	}
	for _, call := range f.provider.calls {
		if call != "delete" {
			t.Fatal("known-ID cleanup used discovery", call)
		}
	}
	f.provider.exists = true
	if err := f.restartAccount().CloseExisting(context.Background(), "legacy-id"); err != nil || f.provider.exists {
		t.Fatal("late saved client survived", err)
	}
}
