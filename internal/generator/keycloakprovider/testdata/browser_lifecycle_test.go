package keycloak

import (
	"context"
	"encoding/json"
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
func (p *browserLifecycleFixture) InspectBrowserClientAccess(ctx context.Context, b ClientBinding, _ BrowserAccessPolicy) error {
	if _, err := p.InspectClient(ctx, b); err != nil {
		return err
	}
	if !p.enabled {
		return ErrClientConfiguration
	}
	return nil
}
func (p *browserLifecycleFixture) GetClientSecret(context.Context, ClientBinding) (Secret, error) {
	return Secret{"fixture-browser-credential"}, nil
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

type browserCredentialFixture struct {
	*browserLifecycleFixture
	inspectCalls, reads int
	failInspect         int
	afterRead           func()
}

func (p *browserCredentialFixture) InspectBrowserClientAccess(ctx context.Context, b ClientBinding, policy BrowserAccessPolicy) error {
	p.inspectCalls++
	if p.inspectCalls == p.failInspect {
		return ErrClientConfiguration
	}
	return p.browserLifecycleFixture.InspectBrowserClientAccess(ctx, b, policy)
}
func (p *browserCredentialFixture) GetClientSecret(ctx context.Context, b ClientBinding) (Secret, error) {
	p.reads++
	if p.afterRead != nil {
		p.afterRead()
	}
	return p.browserLifecycleFixture.GetClientSecret(ctx, b)
}
func TestBrowserCredentialsRequireStableLivePolicy(t *testing.T) {
	for _, fault := range []string{"", "absent", "closed", "first inspection", "last inspection", "journal change", "journal closure", "cancelled", "invalid policy"} {
		t.Run(fault, func(t *testing.T) {
			_, f := newLifecycleFixture(t, false)
			l := restartBrowser(f)
			binding, err := l.Reconcile(context.Background(), browserLifecyclePolicy)
			if err != nil {
				t.Fatal(err)
			}
			if fault == "absent" {
				f.record.Version = 0
				f.record.Data = nil
			}
			if fault == "closed" {
				if err := l.Close(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			p := &browserCredentialFixture{browserLifecycleFixture: &browserLifecycleFixture{f.provider}}
			l.browser = p
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			policy := browserLifecyclePolicy
			switch fault {
			case "first inspection":
				p.failInspect = 1
			case "last inspection":
				p.failInspect = 2
			case "cancelled":
				p.afterRead = cancel
			case "invalid policy":
				policy = func(b ClientBinding) (BrowserAccessPolicy, error) {
					p, _ := browserLifecyclePolicy(b)
					p.Client.RedirectURI = "http://insecure.example/callback"
					return p, nil
				}
			case "journal change", "journal closure":
				p.afterRead = func() {
					record := f.read()
					if fault == "journal closure" {
						record.Closed = true
					}
					data, err := json.Marshal(record)
					if err != nil {
						t.Fatal(err)
					}
					f.record.Version++
					f.record.Data, err = f.protector.Seal(f.key, f.record.Version, data)
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			saves := f.saves
			got, secret, err := l.Credentials(ctx, policy)
			if fault == "" {
				if err != nil || got.ID != binding.ID || secret.Reveal() == "" || p.inspectCalls != 2 || p.reads != 1 {
					t.Fatal("valid credential read failed", err)
				}
			} else if err == nil || secret.Reveal() != "" || got.ID != "" {
				t.Fatal("invalid state released a credential", fault, err)
			}
			if f.saves != saves {
				t.Fatal("credential read changed recovery state")
			}
			if (fault == "absent" || fault == "closed" || fault == "invalid policy" || fault == "first inspection") && p.reads != 0 {
				t.Fatal("invalid state reached credential endpoint")
			}
		})
	}
}
