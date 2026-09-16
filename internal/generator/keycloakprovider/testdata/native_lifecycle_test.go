package keycloak

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	runtime "example.com/provider/out/controller"
)

type lifecycleFixture struct {
	t         *testing.T
	record    runtime.SealedStateRecord
	protector *runtime.StateProtector
	key       runtime.StateKey
	provider  *lifecycleProvider
	saves     int
	failSave  int
	lostSave  bool
}

func (f *lifecycleFixture) read() nativeLifecycleRecord {
	f.t.Helper()
	value, err := f.protector.Open(f.key, f.record.Version, f.record.Data)
	if err != nil {
		f.t.Fatal(err)
	}
	var record nativeLifecycleRecord
	if json.Unmarshal(value.Reveal(), &record) != nil {
		f.t.Fatal("invalid stored lifecycle")
	}
	return record
}
func (f *lifecycleFixture) restart() *NativeClientLifecycle {
	f.t.Helper()
	journal, err := runtime.NewStateJournal(f.protector, f.key, runtime.StatePersistence{
		Load: func(context.Context) (runtime.SealedStateRecord, error) {
			return runtime.SealedStateRecord{Version: f.record.Version, Data: bytes.Clone(f.record.Data)}, nil
		},
		Save: func(_ context.Context, expected int64, data []byte) (runtime.SealedStateRecord, error) {
			f.saves++
			if expected != f.record.Version {
				return runtime.SealedStateRecord{}, ErrConflict
			}
			if f.saves == f.failSave && !f.lostSave {
				return runtime.SealedStateRecord{}, errors.New("storage unavailable")
			}
			f.record = runtime.SealedStateRecord{Version: expected + 1, Data: bytes.Clone(data)}
			if f.saves == f.failSave {
				return runtime.SealedStateRecord{}, errors.New("storage result lost")
			}
			return f.record, nil
		},
	}, 60<<10)
	if err != nil {
		f.t.Fatal(err)
	}
	c := &NativeClientLifecycle{provider: f.provider, journal: journal, identity: f.provider.identity}
	return c
}
func newLifecycleFixture(t *testing.T, legacy bool) (*NativeClientLifecycle, *lifecycleFixture) {
	t.Helper()
	p, err := runtime.NewStateProtector([][]byte{bytes.Repeat([]byte{1}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	f := &lifecycleFixture{t: t, protector: p, key: runtime.StateKey{Instance: "catalog", Entity: "Entry", ResourceID: "one", Scope: "login"}}
	identity := NativeClientIdentity{ClientID: "catalog-cli", Ownership: map[string]string{"stego.owner.product": "catalog"}}
	if legacy {
		identity.LegacyAttributes = map[string]string{"legacy.product": "catalog"}
		identity.LegacyRenames = map[string]string{"legacy.product": "stego.owner.product"}
	}
	f.provider = &lifecycleProvider{fixture: f, identity: identity}
	if legacy {
		f.provider.binding = ClientBinding{ID: "legacy-id", ClientID: identity.ClientID, Attributes: identity.LegacyAttributes}
		f.provider.exists = true
		f.provider.enabled = true
	}
	return f.restart(), f
}
func lifecyclePolicy(b ClientBinding) (NativeAccessPolicy, error) {
	_, base := nativeInputs()
	return NativeAccessPolicy{Client: base, Roles: []string{"open"}, Scopes: RolePolicy{Clients: []ClientRoleGrant{{Client: b, Names: []string{"open"}}}}, Claims: TokenClaimsPolicy{AudienceClients: []ClientBinding{b}}}, nil
}

type lifecycleProvider struct {
	fixture         *lifecycleFixture
	identity        NativeClientIdentity
	binding         ClientBinding
	exists, enabled bool
	calls           []string
	fault           string
}

func (p *lifecycleProvider) note(call string) { p.calls = append(p.calls, call) }
func (p *lifecycleProvider) FindClient(context.Context, string) (ClientRepresentation, error) {
	p.note("find")
	if !p.exists {
		return ClientRepresentation{}, ErrNotFound
	}
	return ClientRepresentation{}, nil
}
func (p *lifecycleProvider) DiscoverOwnedClient(context.Context, string, map[string]string) (ClientBinding, error) {
	p.note("discover")
	if !p.exists {
		return ClientBinding{}, ErrNotFound
	}
	return p.binding, nil
}
func (p *lifecycleProvider) InspectClient(_ context.Context, b ClientBinding) (ClientRepresentation, error) {
	p.note("inspect")
	if !p.exists {
		return ClientRepresentation{}, ErrNotFound
	}
	if !reflect.DeepEqual(b, p.binding) {
		return ClientRepresentation{}, ErrOwnership
	}
	return ClientRepresentation{}, nil
}
func (p *lifecycleProvider) CreateDisabledNativeClient(_ context.Context, b ClientBinding, _ NativeClientPolicy) (ClientRepresentation, error) {
	p.note("create")
	r := p.fixture.read()
	if r.Phase != "allocated" || r.Closed || !reflect.DeepEqual(r.Binding, b) {
		p.fixture.t.Fatal("create has no saved stable binding")
	}
	p.binding = b
	p.exists = true
	p.enabled = false
	if p.fault == "create result lost" {
		return ClientRepresentation{}, errors.New("create result lost")
	}
	return ClientRepresentation{}, nil
}
func (p *lifecycleProvider) DisableClient(context.Context, ClientBinding) error {
	p.note("disable")
	if p.fixture.record.Version == 0 {
		p.fixture.t.Fatal("disable preceded binding save")
	}
	p.enabled = false
	return nil
}
func (p *lifecycleProvider) PrepareClientOwnershipMigration(_ context.Context, b ClientBinding, renames map[string]string) (ClientOwnershipMigration, error) {
	p.note("prepare")
	if p.enabled {
		p.fixture.t.Fatal("migration prepared while enabled")
	}
	return ClientOwnershipMigration{Prior: b, Renames: renames, fingerprint: strings.Repeat("a", 64)}, nil
}
func (p *lifecycleProvider) MigrateClientOwnership(_ context.Context, m ClientOwnershipMigration) (ClientBinding, error) {
	p.note("migrate")
	r := p.fixture.read()
	if r.Phase != "migration" || r.Migration == "" {
		p.fixture.t.Fatal("migration has no saved checkpoint")
	}
	if !p.exists {
		return ClientBinding{}, ErrNotFound
	}
	b, err := m.NextBinding()
	if err != nil {
		return ClientBinding{}, err
	}
	p.binding = b
	if p.fault == "migration result lost" {
		return ClientBinding{}, errors.New("migration result lost")
	}
	return b, nil
}
func (p *lifecycleProvider) ReconcileNativeClientAccess(_ context.Context, b ClientBinding, _ NativeAccessPolicy) error {
	p.note("access")
	r := p.fixture.read()
	if r.Phase != "bound" || r.Closed || !reflect.DeepEqual(r.Binding, b) {
		p.fixture.t.Fatal("enablement preceded new binding save")
	}
	if !p.exists {
		return ErrNotFound
	}
	p.enabled = true
	return nil
}
func (p *lifecycleProvider) DeleteClient(_ context.Context, b ClientBinding) error {
	p.note("delete")
	if !p.fixture.read().Closed {
		p.fixture.t.Fatal("deletion preceded closed record")
	}
	if p.exists && !reflect.DeepEqual(b, p.binding) {
		return ErrOwnership
	}
	p.exists = false
	p.enabled = false
	return nil
}

func TestNativeLifecyclePersistsEveryEffectBoundary(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		l, f := newLifecycleFixture(t, legacy)
		binding, err := l.Reconcile(context.Background(), lifecyclePolicy)
		if err != nil {
			t.Fatal(err)
		}
		if !f.provider.enabled || !reflect.DeepEqual(binding.Attributes, l.identity.Ownership) {
			t.Fatal("native policy did not converge")
		}
		expected := []string{"find", "inspect", "create", "access"}
		if legacy {
			expected = []string{"discover", "disable", "prepare", "migrate", "access"}
		}
		if !reflect.DeepEqual(f.provider.calls, expected) {
			t.Fatal("effect ordering differs", f.provider.calls)
		}
		f.provider.calls = nil
		if _, err = f.restart().Reconcile(context.Background(), lifecyclePolicy); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(f.provider.calls, []string{"access"}) {
			t.Fatal("restart repeated discovery or migration")
		}
		if err = f.restart().Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		if f.provider.exists || !f.read().Closed {
			t.Fatal("close lost durable cleanup intent")
		}
		// A delayed provider create after confirmed deletion must still be removed.
		f.provider.exists = true
		if err = f.restart().Close(context.Background()); err != nil || f.provider.exists {
			t.Fatal("late client escaped cleanup", err)
		}
		if _, err = f.restart().Reconcile(context.Background(), lifecyclePolicy); !errors.Is(err, ErrClientClosed) {
			t.Fatal("closed client was reopened")
		}
	}
}
func TestNativeLifecycleSaveFailuresStopEffects(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		saves := 2
		if legacy {
			saves = 3
		}
		for fail := 1; fail <= saves; fail++ {
			for _, lost := range []bool{false, true} {
				l, f := newLifecycleFixture(t, legacy)
				f.failSave = fail
				f.lostSave = lost
				if _, err := l.Reconcile(context.Background(), lifecyclePolicy); err == nil {
					t.Fatal("failed save allowed reconciliation")
				}
				for _, call := range f.provider.calls {
					if call == "access" {
						t.Fatal("unconfirmed save allowed enablement")
					}
				}
				if fail == 1 {
					for _, call := range f.provider.calls {
						if call != "find" && call != "discover" {
							t.Fatal("effect preceded first confirmed save")
						}
					}
				}
				f.failSave = 0
				if _, err := f.restart().Reconcile(context.Background(), lifecyclePolicy); err != nil {
					t.Fatal("save failure did not recover", legacy, fail, lost, err)
				}
			}
		}
	}
}
func TestNativeLifecycleUncertainProviderResults(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		l, f := newLifecycleFixture(t, legacy)
		f.provider.fault = "create result lost"
		if legacy {
			f.provider.fault = "migration result lost"
		}
		if _, err := l.Reconcile(context.Background(), lifecyclePolicy); err == nil {
			t.Fatal("uncertain provider result accepted")
		}
		if f.provider.enabled {
			t.Fatal("uncertain provider effect enabled login")
		}
		f.provider.fault = ""
		f.provider.calls = nil
		if _, err := f.restart().Reconcile(context.Background(), lifecyclePolicy); err != nil {
			t.Fatal(err)
		}
		for _, call := range f.provider.calls {
			if call == "create" || call == "find" || call == "discover" {
				t.Fatal("uncertain result caused a repeated create or discovery")
			}
		}
	}
}
func TestNativeLifecycleClosedMigrationAndInvalidState(t *testing.T) {
	l, f := newLifecycleFixture(t, true)
	f.provider.fault = "migration result lost"
	if _, err := l.Reconcile(context.Background(), lifecyclePolicy); err == nil {
		t.Fatal("migration fixture did not fail")
	}
	f.provider.fault = ""
	f.provider.calls = nil
	if err := f.restart().Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.provider.calls, []string{"migrate", "delete"}) || f.provider.enabled {
		t.Fatal("closed migration granted access")
	}
	// Valid encryption does not permit a foreign or future lifecycle record.
	for _, change := range []func(*nativeLifecycleRecord){
		func(r *nativeLifecycleRecord) { r.Version = 2 }, func(r *nativeLifecycleRecord) { r.Binding.ClientID = "foreign" },
		func(r *nativeLifecycleRecord) {
			r.Binding.Attributes = map[string]string{"stego.owner.product": "other"}
		},
		func(r *nativeLifecycleRecord) { r.Phase = "unknown" },
	} {
		record := f.read()
		change(&record)
		raw, _ := json.Marshal(record)
		data, err := f.protector.Seal(f.key, f.record.Version, raw)
		if err != nil {
			t.Fatal(err)
		}
		before := len(f.provider.calls)
		original := f.record.Data
		f.record.Data = data
		if err := f.restart().Close(context.Background()); err == nil {
			t.Fatal("invalid lifecycle state accepted")
		}
		if len(f.provider.calls) != before {
			t.Fatal("invalid state caused provider access")
		}
		f.record.Data = original
	}
}
