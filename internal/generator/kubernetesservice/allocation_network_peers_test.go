package kubernetesservice

import (
	"strings"
	"testing"
)

func allocatedPeer() object {
	return object{"direction": "egress", "namespace": "control", "pod_label": "app", "pod_value": "database", "port": 5432, "protocol": "TCP"}
}

func TestAllocationNetworkPeerValidation(t *testing.T) {
	cases := map[string]func(object){
		"direction":           func(p object) { p["direction"] = "both" },
		"namespace":           func(p object) { p["namespace"] = "all" },
		"namespace type":      func(p object) { p["namespace"] = true },
		"external missing":    func(p object) { p["namespace"] = "external" },
		"external template":   func(p object) { p["namespace"] = "external"; p["external_namespace"] = "{{.Namespace}}" },
		"external on control": func(p object) { p["external_namespace"] = "other" },
		"external null":       func(p object) { p["external_namespace"] = nil },
		"label wildcard":      func(p object) { p["pod_label"] = "*" },
		"label empty":         func(p object) { p["pod_label"] = "" },
		"value empty":         func(p object) { p["pod_value"] = "" },
		"value wildcard":      func(p object) { p["pod_value"] = "*" },
		"port zero":           func(p object) { p["port"] = 0 },
		"port high":           func(p object) { p["port"] = 65536 },
		"port named":          func(p object) { p["port"] = "postgres" },
		"protocol":            func(p object) { p["protocol"] = "SCTP" },
		"missing protocol":    func(p object) { delete(p, "protocol") },
		"extra selector":      func(p object) { p["allocation_profile"] = "tenant" },
		"IP rule":             func(p object) { p["cidr"] = "0.0.0.0/0" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p := allocatedPeer()
			mutate(p)
			if _, err := allocationNetworkPeers([]any{p}, true); err == nil {
				t.Fatal("invalid peer accepted")
			}
		})
	}
	for _, raw := range []any{nil, true, []any{}, []any{nil}, []any{allocatedPeer(), allocatedPeer()}, make([]any, 17)} {
		if _, err := allocationNetworkPeers(raw, true); err == nil {
			t.Fatal("invalid peer list accepted")
		}
	}
	if _, err := allocationNetworkPeers([]any{allocatedPeer()}, false); err == nil {
		t.Fatal("peer accepted without isolation")
	}
	for _, namespace := range []string{"control", "allocated", "external"} {
		p := allocatedPeer()
		p["namespace"] = namespace
		if namespace == "external" {
			p["external_namespace"] = "operator-system"
		}
		if _, err := allocationNetworkPeers([]any{p}, true); err != nil {
			t.Fatal(err)
		}
	}
	c := allocationContext()
	p := c.ComponentConfig["allocation_profiles"].([]any)[0].(object)
	p["network_peers"] = []any{allocatedPeer()}
	if _, err := allocationConfig(c); err == nil {
		t.Fatal("configuration ignores isolation requirement")
	}
	p["network_isolation"] = true
	config, err := allocationConfig(c)
	if err != nil {
		t.Fatal(err)
	}
	if len(config.Profiles[0].NetworkPeers) != 1 {
		t.Fatal("configuration lost peers")
	}
	files, err := allocationFiles(c)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range files {
		if strings.Contains(string(f.Bytes()), `\"NetworkPeers\":[`) {
			found = true
		}
	}
	if !found {
		t.Fatal("runtime configuration lost peers")
	}
}

func TestAllocationNetworkPeerAdmissionRules(t *testing.T) {
	p := allocationProfile{NetworkIsolation: true}
	for _, ns := range []string{"control", "allocated", "external"} {
		peer := allocatedPeer()
		peer["namespace"] = ns
		if ns == "external" {
			peer["external_namespace"] = "operator-system"
		}
		parsed, err := allocationNetworkPeers([]any{peer}, true)
		if err != nil {
			t.Fatal(err)
		}
		p.NetworkPeers = append(p.NetworkPeers, parsed...)
	}
	expression := allocationNetworkRulesCEL(p, "egress")
	for _, required := range []string{
		`size(variables.o.spec.egress) == 3`,
		`variables.o.spec.egress[0].to[0].namespaceSelector.matchLabels["kubernetes.io/metadata.name"] == "{{.Namespace}}"`,
		`variables.o.spec.egress[1].to[0].namespaceSelector.matchLabels["kubernetes.io/metadata.name"] == namespaceObject.metadata.name`,
		`variables.o.spec.egress[2].to[0].namespaceSelector.matchLabels["kubernetes.io/metadata.name"] == "operator-system"`,
		`size(variables.o.spec.egress[0].to) == 1`,
		`size(variables.o.spec.egress[0].to[0].podSelector.matchLabels) == 1`,
		`variables.o.spec.egress[0].to[0].podSelector.matchLabels["app"] == "database"`,
		`!has(variables.o.spec.egress[0].to[0].ipBlock)`,
		`size(variables.o.spec.egress[0].ports) == 1`,
		`variables.o.spec.egress[0].ports[0].port == 5432`,
		`variables.o.spec.egress[0].ports[0].protocol == "TCP"`,
		`!has(variables.o.spec.egress[0].ports[0].endPort)`,
	} {
		if !strings.Contains(expression, required) {
			t.Fatal("missing constraint", required)
		}
	}
	if got := allocationNetworkRulesCEL(p, "ingress"); !strings.Contains(got, "size(variables.o.spec.ingress) == 0") {
		t.Fatal("undeclared ingress permitted")
	}
}
