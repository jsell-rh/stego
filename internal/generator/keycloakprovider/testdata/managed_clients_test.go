package keycloak

import (
	"errors"
	"testing"
)

// The fixture declares three kinds: service-account (two ids, legacy form,
// legacy stored name), gateway (one id, legacy form, no legacy name), and
// console (one id, no legacy form).

func TestManagedClientIdentity(t *testing.T) {
	identity, err := ServiceAccountClientIdentity("gw-1", "acct-1")
	if err != nil {
		t.Fatal(err)
	}
	if identity.ClientID != "hs-sa-gw-1-acct-1" {
		t.Fatal(identity.ClientID)
	}
	if identity.Ownership["stego.owner.hypershell.service-account"] != "true" ||
		identity.Ownership["stego.owner.hypershell.gateway-id"] != "gw-1" ||
		identity.Ownership["stego.owner.hypershell.service-account-id"] != "acct-1" {
		t.Fatal(identity.Ownership)
	}
	if identity.LegacyAttributes["hypershell.service-account"] != "true" ||
		identity.LegacyAttributes["hypershell.gateway-id"] != "gw-1" ||
		identity.LegacyAttributes["hypershell.service-account-id"] != "acct-1" {
		t.Fatal(identity.LegacyAttributes)
	}
	if identity.LegacyRenames["hypershell.service-account"] != "stego.owner.hypershell.service-account" {
		t.Fatal(identity.LegacyRenames)
	}
	gateway, err := GatewayClientIdentity("gw-1")
	if err != nil {
		t.Fatal(err)
	}
	if gateway.ClientID != "hs-gateway-gw-1" {
		t.Fatal(gateway.ClientID)
	}
	console, err := ConsoleClientIdentity("gw-1")
	if err != nil {
		t.Fatal(err)
	}
	if console.ClientID != "hs-console-gw-1" || console.LegacyAttributes != nil || console.LegacyRenames != nil {
		t.Fatal(console)
	}
	if _, err := ServiceAccountClientIdentity("", "acct-1"); !errors.Is(err, ErrOwnership) {
		t.Fatal(err)
	}
	if _, err := GatewayClientIdentity(""); !errors.Is(err, ErrOwnership) {
		t.Fatal(err)
	}
}

func TestManagedClientBinding(t *testing.T) {
	live := func(clientID string, attributes map[string]string) *ClientRepresentation {
		return &ClientRepresentation{ID: "uuid-1", ClientID: clientID, Attributes: attributes}
	}
	current := map[string]string{
		"stego.owner.hypershell.service-account":    "true",
		"stego.owner.hypershell.gateway-id":         "gw-1",
		"stego.owner.hypershell.service-account-id": "acct-1",
	}
	legacy := map[string]string{
		"hypershell.service-account":    "true",
		"hypershell.gateway-id":         "gw-1",
		"hypershell.service-account-id": "acct-1",
	}
	binding, err := ServiceAccountClientBinding(live("hs-sa-gw-1-acct-1", current), "gw-1", "acct-1")
	if err != nil {
		t.Fatal(err)
	}
	if binding.ID != "uuid-1" || binding.ClientID != "hs-sa-gw-1-acct-1" {
		t.Fatal(binding)
	}
	if _, err := ServiceAccountClientBinding(live("hs-sa-gw-1-acct-1", legacy), "gw-1", "acct-1"); err != nil {
		t.Fatal(err)
	}
	for name, rejected := range map[string]struct {
		clientID   string
		attributes map[string]string
		ids        []string
	}{
		"nil client":        {"", nil, []string{"gw-1", "acct-1"}},
		"foreign client":    {"other-name", current, []string{"gw-1", "acct-1"}},
		"mixed ownership":   {"hs-sa-gw-1-acct-1", merge(current, legacy), []string{"gw-1", "acct-1"}},
		"partial ownership": {"hs-sa-gw-1-acct-1", pick(current, "stego.owner.hypershell.service-account"), []string{"gw-1", "acct-1"}},
		"wrong account id":  {"hs-sa-gw-1-acct-1", current, []string{"gw-1", "acct-2"}},
	} {
		if _, err := ServiceAccountClientBinding(live(rejected.clientID, rejected.attributes), rejected.ids[0], rejected.ids[1]); err == nil {
			t.Fatal(name + " accepted")
		} else if !errors.Is(err, ErrOwnership) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := ConsoleClientBinding(live("hs-console-gw-1", map[string]string{
		"stego.owner.hypershell.console":    "true",
		"stego.owner.hypershell.gateway-id": "gw-1",
	}), "gw-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := ConsoleClientBinding(live("hs-console-gw-1", nil), "gw-1"); !errors.Is(err, ErrOwnership) {
		t.Fatal("console client without ownership keys accepted")
	}
	if _, err := ConsoleClientBinding(live("hs-console-gw-1", map[string]string{
		"stego.owner.hypershell.console": "true",
	}), "gw-1"); !errors.Is(err, ErrOwnership) {
		t.Fatal("console client with foreign owner key accepted")
	}
}

func TestManagedAudienceBinding(t *testing.T) {
	legacy := map[string]string{
		"hypershell.gateway":    "true",
		"hypershell.gateway-id": "gw-1",
	}
	current := map[string]string{
		"stego.owner.hypershell.gateway":    "true",
		"stego.owner.hypershell.gateway-id": "gw-1",
	}
	live := &ClientRepresentation{ID: "uuid-2", ClientID: "old-public-name", Attributes: legacy}
	if _, err := GatewayAudienceBinding(live, "gw-1", "old-public-name"); err != nil {
		t.Fatal(err)
	}
	migrated := &ClientRepresentation{ID: "uuid-2", ClientID: "hs-gateway-gw-1", Attributes: current}
	if _, err := GatewayAudienceBinding(migrated, "gw-1", "old-public-name"); !errors.Is(err, ErrOwnership) {
		t.Fatal("current ownership accepted a legacy stored name")
	}
	if _, err := GatewayClientBinding(migrated, "gw-1"); err != nil {
		t.Fatal(err)
	}
}

func merge(sources ...map[string]string) map[string]string {
	merged := map[string]string{}
	for _, source := range sources {
		for key, value := range source {
			merged[key] = value
		}
	}
	return merged
}

func pick(source map[string]string, key string) map[string]string {
	return map[string]string{key: source[key]}
}
