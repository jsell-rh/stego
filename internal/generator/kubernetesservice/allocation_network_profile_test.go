package kubernetesservice

import (
	"maps"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
)

func networkProfileContext() gen.Context {
	c := allocationContext()
	p := c.ComponentConfig["allocation_profiles"].([]any)[0].(object)
	p["network_isolation"] = true
	peer := maps.Clone(p)
	peer["name"] = "peer"
	peer["namespace_prefix"] = "peer-"
	rule := allocatedPeer()
	rule["namespace"] = "profile"
	rule["peer_profile"] = "peer"
	p["network_peers"] = []any{rule}
	c.ComponentConfig["allocation_profiles"] = []any{p, peer}
	return c
}

func TestAllocationNetworkProfileValidation(t *testing.T) {
	for name, change := range map[string]func(object, object){
		"unknown":                func(p, peer object) { p["network_peers"].([]any)[0].(object)["peer_profile"] = "missing" },
		"self":                   func(p, peer object) { p["network_peers"].([]any)[0].(object)["peer_profile"] = p["name"] },
		"different owner domain": func(p, peer object) { peer["owner_label"] = "example.test/other" },
		"different suffix":       func(p, peer object) { peer["suffix_length"] = 9 },
		"unisolated target":      func(p, peer object) { delete(peer, "network_isolation") },
		"missing target":         func(p, peer object) { delete(p["network_peers"].([]any)[0].(object), "peer_profile") },
		"external selector":      func(p, peer object) { p["network_peers"].([]any)[0].(object)["external_namespace"] = "other" },
		"wrong type":             func(p, peer object) { p["network_peers"].([]any)[0].(object)["peer_profile"] = true },
		"target on local peer":   func(p, peer object) { p["network_peers"].([]any)[0].(object)["namespace"] = "allocated" },
	} {
		t.Run(name, func(t *testing.T) {
			c := networkProfileContext()
			profiles := c.ComponentConfig["allocation_profiles"].([]any)
			change(profiles[0].(object), profiles[1].(object))
			files, wiring, err := new(Generator).Generate(c)
			if err == nil || files != nil || wiring != nil {
				t.Fatal("invalid related network peer emitted output")
			}
		})
	}
	c := networkProfileContext()
	config, err := allocationConfig(c)
	if err != nil {
		t.Fatal(err)
	}
	p := config.Profiles[0]
	peer := p.NetworkPeers[0]
	if peer.PeerPrefix != "peer-" || peer.PeerProfile != "peer" {
		t.Fatal("related profile was not resolved", peer)
	}
	expression := allocationNetworkRulesCEL(p, "egress")
	for _, part := range []string{"size(variables.o.spec.egress[0].to[0].namespaceSelector.matchLabels) == 4", `"stego.dev/allocator"`, `"stego.dev/allocation-profile"`, `"example.test/owner"`, `"peer-" + namespaceObject.metadata.name.substring(7)`} {
		if !strings.Contains(expression, part) {
			t.Fatal("related network guard lost a bound", part, expression)
		}
	}
}
