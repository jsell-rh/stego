package keycloak

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

type nativeAccessFixture struct {
	*roleFixture
	fault       string
	enabledOnce bool
	cancel      context.CancelFunc
}

func newNativeAccessFixture(t *testing.T) (*Client, *nativeAccessFixture, ClientBinding, NativeAccessPolicy) {
	t.Helper()
	c, roles := newMapperFixture(t)
	f := &nativeAccessFixture{roleFixture: roles}
	b, base := nativeInputs()
	b.ID = "worker"
	p := NativeAccessPolicy{Client: base, Roles: []string{"open"}, Scopes: RolePolicy{Clients: []ClientRoleGrant{{Client: b, Names: []string{"open"}}}}, Claims: TokenClaimsPolicy{AudienceClients: []ClientBinding{b}, ClientRoles: []ClientRoleClaim{{Client: b, Claim: "catalog.roles"}}}}
	value, err := nativeClientConfiguration(b, base)
	if err != nil {
		t.Fatal(err)
	}
	value.Name = "old name"
	f.clients[b.ID] = value
	f.intercept = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/admin/realms/tenant/clients/worker" {
			return false
		}
		value := f.clients[b.ID]
		if r.Method == http.MethodGet {
			value.DefaultClientScopes = []string{}
			value.OptionalClientScopes = []string{}
			for _, s := range f.scopes["default-client-scopes"] {
				value.DefaultClientScopes = append(value.DefaultClientScopes, s.Name)
			}
			for _, s := range f.scopes["optional-client-scopes"] {
				value.OptionalClientScopes = append(value.OptionalClientScopes, s.Name)
			}
			_ = json.NewEncoder(w).Encode(value)
			return true
		}
		if r.Method != http.MethodPut {
			t.Error("unexpected native access method")
			w.WriteHeader(500)
			return true
		}
		var update map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&update) != nil {
			t.Error("invalid native access write")
			w.WriteHeader(400)
			return true
		}
		enable := string(update["enabled"]) == "true"
		f.writes = append(f.writes, "PUT enabled="+string(update["enabled"]))
		if enable {
			if value.Enabled || len(update) != 2 || string(update["webOrigins"]) != "[]" {
				t.Error("unsafe native enable request")
			}
			if f.fault == "ignored enable" {
				w.WriteHeader(204)
				return true
			}
		} else if f.enabledOnce && f.fault == "failed rollback" {
			w.WriteHeader(503)
			return true
		}
		data, _ := json.Marshal(value)
		var record map[string]json.RawMessage
		_ = json.Unmarshal(data, &record)
		for key, v := range update {
			if key == "attributes" {
				var patch map[string]string
				_ = json.Unmarshal(v, &patch)
				for k, a := range patch {
					if a == "" {
						delete(value.Attributes, k)
					} else {
						value.Attributes[k] = a
					}
				}
				record[key], _ = json.Marshal(value.Attributes)
			} else {
				record[key] = v
			}
		}
		data, _ = json.Marshal(record)
		_ = json.Unmarshal(data, &value)
		if enable {
			f.enabledOnce = true
			switch f.fault {
			case "changed owner":
				value.Attributes = map[string]string{"stego.owner.product": "other"}
			case "post-enable drift", "failed rollback":
				value.Attributes["unexpected.setting"] = "true"
			case "post-enable read failure":
				f.malformedMappers = true
			case "canceled enable":
				f.cancel()
			}
		}
		f.clients[b.ID] = value
		if enable && f.fault == "uncertain enable" {
			w.WriteHeader(503)
		} else {
			w.WriteHeader(204)
		}
		return true
	}
	return c, f, b, p
}

func TestNativeAccessReconciliation(t *testing.T) {
	c, f, b, p := newNativeAccessFixture(t)
	// The compound operation must use one permit, including its child steps.
	for i := 0; i < MaxConcurrentOperations-1; i++ {
		c.permits <- struct{}{}
	}
	defer func() {
		for i := 0; i < MaxConcurrentOperations-1; i++ {
			<-c.permits
		}
	}()
	if err := c.ReconcileNativeClientAccess(context.Background(), b, p); err != nil {
		t.Fatal(err)
	}
	if !f.clients[b.ID].Enabled {
		t.Fatal("native access was not enabled")
	}
	count := len(f.writes)
	if err := c.InspectNativeClientAccess(context.Background(), b, p); err != nil {
		t.Fatal(err)
	}
	if err := c.ReconcileNativeClientAccess(context.Background(), b, p); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != count {
		t.Fatal("correct enabled policy caused writes")
	}
	v := f.clients[b.ID]
	v.Name = "drift"
	v.FullScopeAllowed = true
	f.clients[b.ID] = v
	f.scopes["default-client-scopes"] = []assignedScope{{ID: "injected", Name: "injected"}}
	if err := c.ReconcileNativeClientAccess(context.Background(), b, p); err != nil {
		t.Fatal(err)
	}
	if f.writes[count] != "PUT enabled=false" || f.writes[len(f.writes)-1] != "PUT enabled=true" {
		t.Fatal("repair did not occur between disablement and enablement")
	}
	if len(f.scopes["default-client-scopes"]) != 0 {
		t.Fatal("shared scope remains")
	}
}

func TestNativeAccessFailureCleanup(t *testing.T) {
	for _, fault := range []string{"ignored enable", "uncertain enable", "post-enable drift", "post-enable read failure", "failed rollback", "changed owner", "canceled enable"} {
		t.Run(fault, func(t *testing.T) {
			c, f, b, p := newNativeAccessFixture(t)
			f.fault = fault
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f.cancel = cancel
			err := c.ReconcileNativeClientAccess(ctx, b, p)
			if err == nil {
				t.Fatal("unconfirmed access accepted")
			}
			unconfirmed := fault == "failed rollback" || fault == "changed owner"
			if errors.Is(err, ErrAccessDisablementUnconfirmed) != unconfirmed {
				t.Fatal("disablement result differs", err)
			}
			if !unconfirmed && f.clients[b.ID].Enabled {
				t.Fatal("failed check left access enabled")
			}
			if fault == "canceled enable" && !errors.Is(err, context.Canceled) {
				t.Fatal("caller cancellation was lost", err)
			}
			enables := 0
			for _, w := range f.writes {
				if w == "PUT enabled=true" {
					enables++
				}
			}
			if enables != 1 {
				t.Fatal("enable was replayed", enables)
			}
			if fault == "changed owner" && f.writes[len(f.writes)-1] != "PUT enabled=true" {
				t.Fatal("foreign client was changed during cleanup")
			}
		})
	}
}

func TestNativeAccessRejectsIncompletePolicy(t *testing.T) {
	for _, fault := range []string{"scope removal", "mapper removal", "enabled inspection", "foreign owner", "no audience"} {
		t.Run(fault, func(t *testing.T) {
			c, f, b, p := newNativeAccessFixture(t)
			switch fault {
			case "scope removal":
				f.scopes["default-client-scopes"] = []assignedScope{{ID: "old", Name: "old"}}
				f.ignoreScopeDelete = true
			case "mapper removal":
				f.ignoreDelete = true
			case "enabled inspection":
				if err := c.ReconcileNativeClientAccess(context.Background(), b, p); err != nil {
					t.Fatal(err)
				}
				f.writes = nil
				f.malformedScopes = true
			case "foreign owner":
				v := f.clients[b.ID]
				v.Attributes = map[string]string{"stego.owner.product": "other"}
				f.clients[b.ID] = v
			case "no audience":
				p.Claims.AudienceClients = nil
			}
			if err := c.ReconcileNativeClientAccess(context.Background(), b, p); err == nil {
				t.Fatal("incomplete policy accepted")
			}
			if fault != "foreign owner" && f.clients[b.ID].Enabled {
				t.Fatal("failed policy retained access")
			}
			for _, write := range f.writes {
				if strings.Contains(write, "enabled=true") {
					t.Fatal("incomplete policy enabled client")
				}
			}
			if (fault == "foreign owner" || fault == "no audience") && len(f.writes) != 0 {
				t.Fatal("invalid policy changed provider state")
			}
		})
	}
}
