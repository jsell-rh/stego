package keycloak

import (
	"context"
	"errors"
	"testing"
)

type browserLifecycleFixture struct{ *lifecycleProvider }

func (p *browserLifecycleFixture) CreateDisabledBrowserClient(ctx context.Context, b ClientBinding, _ BrowserClientPolicy) (ClientRepresentation, error) {
	return p.CreateDisabledNativeClient(ctx, b, NativeClientPolicy{})
}
func (p *browserLifecycleFixture) ReconcileBrowserClientAccess(ctx context.Context, b ClientBinding, _ BrowserAccessPolicy) error {
	return p.ReconcileNativeClientAccess(ctx, b, NativeAccessPolicy{})
}
func restartBrowser(f *lifecycleFixture) *BrowserClientLifecycle {
	base := f.restart().clientLifecycle
	base.kind = "browser"
	return &BrowserClientLifecycle{clientLifecycle: base, browser: &browserLifecycleFixture{f.provider}}
}
func browserLifecyclePolicy(b ClientBinding) (BrowserAccessPolicy, error) {
	_, base := browserInputs()
	return BrowserAccessPolicy{Client: base, Roles: []string{"open"}, Scopes: RolePolicy{Clients: []ClientRoleGrant{{Client: b, Names: []string{"open"}}}}, Claims: TokenClaimsPolicy{AudienceClients: []ClientBinding{b}}}, nil
}
func TestBrowserLifecycleRecoveryAndKind(t *testing.T) {
	for _, fault := range []string{"save failure", "create result lost", ""} {
		t.Run(fault, func(t *testing.T) {
			_, f := newLifecycleFixture(t, false)
			l := restartBrowser(f)
			if fault == "save failure" {
				f.failSave = 1
			}
			if fault == "create result lost" {
				f.provider.fault = fault
			}
			binding, err := l.Reconcile(context.Background(), browserLifecyclePolicy)
			if fault != "" && err == nil {
				t.Fatal("missing injected failure")
			}
			if fault == "save failure" && f.provider.exists {
				t.Fatal("create preceded durable intent")
			}
			if fault == "" && err != nil {
				t.Fatal(err)
			}
			f.failSave = 0
			f.provider.fault = ""
			recovered, err := restartBrowser(f).Reconcile(context.Background(), browserLifecyclePolicy)
			if err != nil || !f.provider.enabled || recovered.ID == "" {
				t.Fatal("browser recovery failed", err)
			}
			if binding.ID != "" && binding.ID != recovered.ID {
				t.Fatal("browser identity changed after restart")
			}
			if f.read().Kind != "browser" {
				t.Fatal("browser journal kind missing")
			}
			if _, err = f.restart().Reconcile(context.Background(), lifecyclePolicy); !errors.Is(err, ErrClientLifecycle) {
				t.Fatal("native lifecycle consumed browser record", err)
			}
			if err = restartBrowser(f).Close(context.Background()); err != nil || f.provider.exists {
				t.Fatal("browser close failed", err)
			}
			if _, err = restartBrowser(f).Reconcile(context.Background(), browserLifecyclePolicy); err == nil {
				t.Fatal("closed browser identity reopened")
			}
			f.provider.exists = true
			f.provider.enabled = false
			if err = restartBrowser(f).Close(context.Background()); err != nil || f.provider.exists {
				t.Fatal("late create was not removed", err)
			}
		})
	}
}
func TestBrowserLifecycleRejectsPolicyBeforeCreate(t *testing.T) {
	_, f := newLifecycleFixture(t, false)
	_, err := restartBrowser(f).Reconcile(context.Background(), func(b ClientBinding) (BrowserAccessPolicy, error) {
		p, _ := browserLifecyclePolicy(b)
		p.Client.PostLogoutRedirectURI = "https://foreign.example.test/"
		return p, nil
	})
	if err == nil || f.provider.exists {
		t.Fatal("invalid browser policy created a provider client")
	}
}
