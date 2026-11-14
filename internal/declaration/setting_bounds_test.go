package declaration

import (
	"strings"
	"testing"
)

func TestSettingBounds(t *testing.T) {
	const inv87 = "ST-DECL-INV87-T A ledger or controller setting beyond its range is refused"
	ledger := func(old, new string) []string { return []string{old, new} }
	for _, c := range []struct {
		name, old, new, path, found string
	}{
		{"pending timeout 8760h", "pending_timeout: 15m", "pending_timeout: 8760h", "", ""},
		{"pending timeout 8761h", "pending_timeout: 15m", "pending_timeout: 8761h", "entities[1].ledger.pending_timeout", "8761h"},
		{"snapshot every 10000", "snapshot_every: 3", "snapshot_every: 10000", "", ""},
		{"snapshot every 10001", "snapshot_every: 3", "snapshot_every: 10001", "entities[1].ledger.snapshot_every", "10001"},
	} {
		t.Run(inv87+": "+c.name, func(t *testing.T) {
			faults := ruleFaults(td04Changed(t, ledger(c.old, c.new)...), "setting-bound")
			if c.path == "" {
				if len(faults) != 0 {
					t.Errorf("want no fault, got %+v", faults)
				}
				return
			}
			if len(faults) != 1 || faults[0].Path != c.path || faults[0].Found != c.found || !strings.Contains(faults[0].Message, `"Wallet"`) {
				t.Errorf("want one fault at %s that names Wallet and %s, got %+v", c.path, c.found, faults)
			}
		})
	}
	for _, c := range []struct {
		name, old, new, field string
	}{
		{"resync 1ms", "resync: 10m", "resync: 1ms", ""},
		{"resync 24h", "resync: 10m", "resync: 24h", ""},
		{"resync 999us", "resync: 10m", "resync: 999us", "resync"},
		{"resync 25h", "resync: 10m", "resync: 25h", "resync"},
		{"lease TTL 5m", "lease_ttl: 15s", "lease_ttl: 5m", ""},
		{"lease TTL 14s", "lease_ttl: 15s", "lease_ttl: 14s", "lease_ttl"},
		{"lease TTL 301s", "lease_ttl: 15s", "lease_ttl: 301s", "lease_ttl"},
		{"replicas 10", "replicas: 2", "replicas: 10", ""},
		{"replicas 11", "replicas: 2", "replicas: 11", "replicas"},
	} {
		t.Run(inv87+": "+c.name, func(t *testing.T) {
			faults := ruleFaults(td02Composition(t, "", strings.Replace(provisionerWithout(""), c.old, c.new, 1)), "setting-bound")
			if c.field == "" {
				if len(faults) != 0 {
					t.Errorf("want no fault, got %+v", faults)
				}
				return
			}
			found := strings.TrimPrefix(c.new, c.field+": ")
			if len(faults) != 1 || faults[0].Path != "controllers[0]."+c.field || faults[0].Found != found || !strings.Contains(faults[0].Message, `"provisioner"`) {
				t.Errorf("want one fault at controllers[0].%s that names provisioner and %s, got %+v", c.field, found, faults)
			}
		})
	}
	t.Run(inv87+": a value out of form gets only the fault of its form", func(t *testing.T) {
		if faults := ruleFaults(td02Composition(t, "", strings.Replace(provisionerWithout(""), "resync: 10m", "resync: -1h", 1)), "setting-bound"); len(faults) != 0 {
			t.Errorf("want no bound fault for a negative resync, got %+v", faults)
		}
	})
}
