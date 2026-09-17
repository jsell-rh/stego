package kubernetesservice

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAllocationServiceAccountValidation(t *testing.T) {
	for name, aliases := range map[string]any{
		"empty": []any{}, "null": nil, "string": "gateway", "number": []any{1},
		"duplicate": []any{"gateway", "gateway"}, "default": []any{"default"},
		"long": []any{"gatewaytoolong"}, "path": []any{"../gateway"},
		"uppercase": []any{"Gateway"}, "unbound": []any{"reader"},
		"too_many": []any{"one", "two", "three", "four", "five", "six", "seven", "eight", "nine"},
	} {
		t.Run(name, func(t *testing.T) {
			c := allocationContext()
			c.ComponentConfig["allocation_profiles"].([]any)[0].(object)["service_accounts"] = aliases
			if _, err := allocationConfig(c); err == nil {
				t.Fatal("invalid ServiceAccount declaration accepted")
			}
		})
	}
	c := allocationContext()
	c.ComponentConfig["allocation_profiles"].([]any)[0].(object)["service_accounts"] = []any{"gateway"}
	config, err := allocationConfig(c)
	if err != nil {
		t.Fatal(err)
	}
	if len(config.Profiles[0].ServiceAccounts) != 1 || config.Profiles[0].ServiceAccounts[0] != "gateway" {
		t.Fatal("account declaration was lost")
	}
	items, err := allocationObjects(config)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{".service-accounts", "stego.dev/service-account-gateway", "automountServiceAccountToken", "[a-z2-7]{52}"} {
		if !strings.Contains(string(raw), part) {
			t.Fatal("account guard missing", part)
		}
	}
	for _, item := range items {
		m := item.(object)
		if m["kind"] == "ValidatingAdmissionPolicy" && m["spec"].(object)["failurePolicy"] != "Fail" {
			t.Fatal("policy permits admission failure")
		}
	}
}

func TestAllocationServiceAccountManifests(t *testing.T) {
	t.Setenv("STEGO_ALLOCATION_SERVICE_ACCOUNTS", "1")
	TestAllocationManifests(t)
}
