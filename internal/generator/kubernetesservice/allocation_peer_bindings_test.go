package kubernetesservice

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
)

func peerAllocationContext() gen.Context {
	c := allocationContext()
	p := c.ComponentConfig["allocation_profiles"].([]any)[0].(object)
	p["service_accounts"] = []any{"gateway"}
	peer := object{}
	for key, value := range p {
		peer[key] = value
	}
	peer["name"] = "jobs"
	peer["namespace_prefix"] = "jobs-"
	peer["service_accounts"] = []any{"runner"}
	peer["bindings"] = []any{object{"role": "data", "namespace": "profile", "subject_profile": "tenant", "service_account": "gateway"}}
	c.ComponentConfig["allocation_profiles"] = []any{p, peer}
	return c
}

func TestAllocationPeerBindingsValidation(t *testing.T) {
	c := peerAllocationContext()
	config, err := allocationConfig(c)
	if err != nil {
		t.Fatal(err)
	}
	b := config.Profiles[1].Bindings[0]
	if b.SubjectPrefix != "tenant-" || b.SubjectProfile != "tenant" {
		t.Fatal("related namespace mapping was lost")
	}
	cases := map[string]func(object, object, object){
		"missing profile":        func(p, peer, b object) { delete(b, "subject_profile") },
		"unknown profile":        func(p, peer, b object) { b["subject_profile"] = "missing" },
		"self profile":           func(p, peer, b object) { b["subject_profile"] = "jobs" },
		"null profile":           func(p, peer, b object) { b["subject_profile"] = nil },
		"profile on control":     func(p, peer, b object) { b["namespace"] = "control" },
		"profile on allocated":   func(p, peer, b object) { b["namespace"] = "allocated" },
		"external namespace":     func(p, peer, b object) { b["external_namespace"] = "foreign" },
		"different owner domain": func(p, peer, b object) { peer["owner_label"] = "other.test/owner" },
		"different suffix":       func(p, peer, b object) { peer["suffix_length"] = 9 },
		"literal source account": func(p, peer, b object) { delete(p, "service_accounts") },
		"legacy target accounts": func(p, peer, b object) { delete(peer, "service_accounts") },
		"unknown alias":          func(p, peer, b object) { b["service_account"] = "missing" },
		"cluster role":           func(p, peer, b object) { b["role"] = "review" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			c := peerAllocationContext()
			profiles := c.ComponentConfig["allocation_profiles"].([]any)
			p, peer := profiles[0].(object), profiles[1].(object)
			b := peer["bindings"].([]any)[0].(object)
			change(p, peer, b)
			if _, err := allocationConfig(c); err == nil {
				t.Fatal("unsafe profile relation accepted")
			}
		})
	}
}

func TestAllocationPeerBindingsManifests(t *testing.T) {
	config, err := allocationConfig(peerAllocationContext())
	if err != nil {
		t.Fatal(err)
	}
	items, err := allocationObjects(config)
	if err != nil {
		t.Fatal(err)
	}
	var expressions []string
	for _, raw := range items {
		item := raw.(object)
		if item["kind"] != "ValidatingAdmissionPolicy" {
			continue
		}
		for _, v := range item["spec"].(object)["validations"].([]any) {
			expressions = append(expressions, v.(object)["expression"].(string))
		}
	}
	text := strings.Join(expressions, "\n")
	for _, part := range []string{"namespaceObject.metadata.name.substring(5)", "stego.dev/service-account-gateway", `subjects[0].namespace == "tenant-" +`, "oldObject.metadata.annotations"} {
		if !strings.Contains(text, part) {
			t.Fatal("related account guard missing", part)
		}
	}
	aliases := allocationAccountIdentities(config.Profiles[1])
	if len(aliases) != 2 || aliases[0] != "runner" || aliases[1] != "gateway" {
		t.Fatal("local and imported account identities were not retained")
	}
	if len(config.Profiles[1].ServiceAccounts) != 1 {
		t.Fatal("imported account became a local account")
	}
	t.Setenv("STEGO_ALLOCATION_SERVICE_ACCOUNTS", "1")
	if dir := os.Getenv("STEGO_ALLOCATION_ARTIFACTS"); dir != "" {
		t.Setenv("STEGO_ALLOCATION_ARTIFACTS", filepath.Join(dir, "related"))
	}
	c := peerAllocationContext()
	if prefix := os.Getenv("STEGO_ALLOCATION_PEER_PREFIX"); prefix != "" {
		c.ComponentConfig["allocation_profiles"].([]any)[1].(object)["namespace_prefix"] = prefix
	}
	testAllocationManifests(t, c)
}
