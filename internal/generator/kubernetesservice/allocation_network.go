package kubernetesservice

import (
	"fmt"
	"strings"
)

func allocationNetworkPeers(raw any, isolated bool) ([]allocationNetworkPeer, error) {
	entries, ok := raw.([]any)
	if !isolated || !ok || len(entries) == 0 || len(entries) > 16 {
		return nil, fmt.Errorf("allocation network_peers requires network_isolation and 1..16 peers")
	}
	result := make([]allocationNetworkPeer, 0, len(entries))
	seen := map[allocationNetworkPeer]bool{}
	for _, entry := range entries {
		fields, ok := entry.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("allocation network peer must be an object")
		}
		for key := range fields {
			switch key {
			case "direction", "namespace", "external_namespace", "pod_label", "pod_value", "port", "protocol":
			default:
				return nil, fmt.Errorf("unknown allocation network peer field %q", key)
			}
		}
		p := allocationNetworkPeer{}
		p.Direction, _ = fields["direction"].(string)
		p.Namespace, _ = fields["namespace"].(string)
		p.PodLabel, _ = fields["pod_label"].(string)
		p.PodValue, _ = fields["pod_value"].(string)
		p.Protocol, _ = fields["protocol"].(string)
		p.Port, ok = fields["port"].(int)
		if !ok || p.Port < 1 || p.Port > 65535 || (p.Direction != "ingress" && p.Direction != "egress") || !validLabelKey(p.PodLabel) || !labelValue.MatchString(p.PodValue) || (p.Protocol != "TCP" && p.Protocol != "UDP") {
			return nil, fmt.Errorf("allocation network peer requires a direction, Pod label and value, numeric port, and TCP or UDP protocol")
		}
		external, exists := fields["external_namespace"]
		switch p.Namespace {
		case "control", "allocated":
			if exists {
				return nil, fmt.Errorf("external_namespace requires an external peer")
			}
		case "external":
			p.ExternalNamespace, ok = external.(string)
			if !ok || !label.MatchString(p.ExternalNamespace) {
				return nil, fmt.Errorf("external network peer requires a literal namespace")
			}
		default:
			return nil, fmt.Errorf("allocation network peer namespace must be control, allocated, or external")
		}
		if seen[p] {
			return nil, fmt.Errorf("duplicate allocation network peer")
		}
		seen[p] = true
		result = append(result, p)
	}
	return result, nil
}

// allocationNetworkRulesCEL checks each complete rule. Namespace and Pod
// selectors belong to the same peer, so both selectors must match.
func allocationNetworkRulesCEL(p allocationProfile, direction string) string {
	path := "variables.o.spec." + direction
	peers := []allocationNetworkPeer{}
	for _, peer := range p.NetworkPeers {
		if peer.Direction == direction {
			peers = append(peers, peer)
		}
	}
	if len(peers) == 0 {
		return "(!has(" + path + ") || size(" + path + ") == 0)"
	}
	checks := []string{fmt.Sprintf("has(%s) && size(%s) == %d", path, path, len(peers))}
	selector := func(path, key, value string) string {
		return "has(" + path + ") && has(" + path + ".matchLabels) && size(" + path + ".matchLabels) == 1 && " + celString(key) + " in " + path + ".matchLabels && " + path + ".matchLabels[" + celString(key) + "] == " + value + " && (!has(" + path + ".matchExpressions) || size(" + path + ".matchExpressions) == 0)"
	}
	for i, peer := range peers {
		rule := fmt.Sprintf("%s[%d]", path, i)
		side := "to"
		if direction == "ingress" {
			side = "from"
		}
		target := rule + "." + side
		ns := celString(peer.ExternalNamespace)
		if peer.Namespace == "control" {
			ns = celString("{{.Namespace}}")
		}
		if peer.Namespace == "allocated" {
			ns = "namespaceObject.metadata.name"
		}
		checks = append(checks, "has("+target+") && size("+target+") == 1 && !has("+target+"[0].ipBlock)",
			selector(target+"[0].namespaceSelector", "kubernetes.io/metadata.name", ns),
			selector(target+"[0].podSelector", peer.PodLabel, celString(peer.PodValue)),
			"has("+rule+".ports) && size("+rule+".ports) == 1 && has("+rule+".ports[0].port) && "+rule+".ports[0].port == "+fmt.Sprint(peer.Port)+" && has("+rule+".ports[0].protocol) && "+rule+".ports[0].protocol == "+celString(peer.Protocol)+" && !has("+rule+".ports[0].endPort)")
	}
	return "(" + strings.Join(checks, " && ") + ")"
}
