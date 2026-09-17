package keycloak

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// The provider has current ownership, while the application also declares the
// legacy format. There is no account journal when the inventory finds the ID.
func newObservedClosureFixture(t *testing.T) *accountLifecycleFixture {
	t.Helper()
	_, f := newAccountLifecycleFixture(t, true)
	f.provider.binding = f.provider.identity.binding("observed-id")
	return f
}

func TestServiceAccountLifecycleObservedClosure(t *testing.T) {
	for _, prepared := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "prepared"}[prepared], func(t *testing.T) {
			f := newObservedClosureFixture(t)
			ctx := context.Background()
			if prepared {
				if err := f.restartAccount().PrepareCloseExisting(ctx, "observed-id"); err != nil {
					t.Fatal(err)
				}
				if len(f.provider.calls) != 0 {
					t.Fatal("preparation reached the provider")
				}
			}
			if err := f.restartAccount().CloseExisting(ctx, "observed-id"); err != nil {
				t.Fatal(err)
			}
			saved := f.read()
			if !saved.Closed || saved.Phase != "bound" || !reflect.DeepEqual(saved.Binding, f.provider.binding) || f.provider.exists {
				t.Fatal("current ownership closure did not retain its ID and finish")
			}
			if !reflect.DeepEqual(f.provider.calls, []string{"delete", "inspect", "delete"}) {
				t.Fatal("closure used unexpected provider operations", f.provider.calls)
			}
			version := f.record.Version
			if err := f.restartAccount().Close(ctx); err != nil || f.record.Version != version {
				t.Fatal("repeat closure changed the journal", err)
			}
			f.provider.exists = true
			if err := f.restartAccount().Close(ctx); err != nil || f.provider.exists {
				t.Fatal("late client survived", err)
			}
			if _, err := f.restartAccount().Reconcile(ctx, accountLifecyclePolicy); !errors.Is(err, ErrClientClosed) {
				t.Fatal("closure reopened", err)
			}
		})
	}
}

func TestServiceAccountLifecycleObservedClosureSaveFailure(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(map[bool]string{false: "rejected", true: "acknowledgement_lost"}[lost], func(t *testing.T) {
			f := newObservedClosureFixture(t)
			f.failSave, f.lostSave = 2, lost
			if err := f.restartAccount().CloseExisting(context.Background(), "observed-id"); err == nil {
				t.Fatal("unconfirmed ownership save accepted")
			}
			if !f.provider.exists || !reflect.DeepEqual(f.provider.calls, []string{"delete", "inspect"}) {
				t.Fatal("unconfirmed save changed the provider")
			}
			if !f.read().Closed {
				t.Fatal("closure intent was lost")
			}
			f.failSave = 0
			if err := f.restartAccount().Close(context.Background()); err != nil || f.provider.exists {
				t.Fatal("saved closure did not recover", err)
			}
		})
	}
}

type observedClosureProvider struct {
	*accountLifecycleProvider
	value              ClientRepresentation
	inspectError       error
	changeAfterInspect bool
}

func (p *observedClosureProvider) InspectClient(ctx context.Context, b ClientBinding) (ClientRepresentation, error) {
	if _, err := p.accountLifecycleProvider.InspectClient(ctx, b); err != nil {
		return ClientRepresentation{}, err
	}
	if p.changeAfterInspect {
		p.binding.Attributes = map[string]string{"stego.owner.product": "other"}
	}
	return p.value, p.inspectError
}

func TestServiceAccountLifecycleObservedClosureRejectsForeignOwnership(t *testing.T) {
	for _, mode := range []string{"provider_id", "client_name", "current_value", "reserved_key", "mixed", "empty_legacy_key", "provider_failure", "changed_after_read"} {
		t.Run(mode, func(t *testing.T) {
			f := newObservedClosureFixture(t)
			b := f.provider.binding
			p := &observedClosureProvider{accountLifecycleProvider: f.account, value: ClientRepresentation{ID: b.ID, ClientID: b.ClientID, Attributes: map[string]string{"stego.owner.product": "catalog"}}}
			switch mode {
			case "provider_id":
				p.value.ID = "foreign-id"
			case "client_name":
				p.value.ClientID = "foreign-client"
			case "current_value":
				p.value.Attributes["stego.owner.product"] = "other"
			case "reserved_key":
				p.value.Attributes["stego.owner.other"] = "foreign"
			case "mixed":
				p.value.Attributes["legacy.product"] = "catalog"
			case "empty_legacy_key":
				p.value.Attributes["legacy.product"] = ""
			case "provider_failure":
				p.inspectError = errors.New("provider unavailable")
			case "changed_after_read":
				p.changeAfterInspect = true
			}
			l := f.restartAccount()
			l.clientLifecycle.provider = p
			if err := l.CloseExisting(context.Background(), b.ID); err == nil {
				t.Fatal("foreign or unconfirmed ownership accepted")
			}
			if !f.provider.exists || !f.read().Closed || f.read().Binding.ID != b.ID {
				t.Fatal("failed closure changed provider or lost identity")
			}
			if mode != "changed_after_read" && (f.read().Phase != "legacy" || f.saves != 1) {
				t.Fatal("unconfirmed ownership was saved")
			}
			for _, call := range f.provider.calls {
				if call != "delete" && call != "inspect" {
					t.Fatal("closure used discovery or enabled access", call)
				}
			}
		})
	}
}
