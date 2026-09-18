package allocation

import (
	"context"
	"errors"
	kube "example.com/widget/out/kubernetes"
	"fmt"
	"strings"
	"testing"
	"time"
)

func selectedNetworkPeers() []networkPeer {
	return []networkPeer{
		{Direction: "ingress", Namespace: "control", PodLabel: "app", PodValue: "controller", Port: 8080, Protocol: "TCP"},
		{Direction: "egress", Namespace: "allocated", PodLabel: "app", PodValue: "database", Port: 5432, Protocol: "TCP"},
		{Direction: "egress", Namespace: "external", ExternalNamespace: "cluster-dns", PodLabel: "app", PodValue: "dns", Port: 53, Protocol: "UDP"},
	}
}
func TestAllocationNetworkPeersLifecycle(t *testing.T) {
	a, s := fixture(t)
	a.config.Profiles[0].NetworkIsolation = true
	a.config.Profiles[0].NetworkPeers = selectedNetworkPeers()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := a.Ensure(ctx, "tenant", networkName, "owner-1"); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	if len(s.writes) != 6 || s.writes[2] != "POST "+strings.TrimSuffix(networkPath, "/stego-allocation") {
		t.Fatal("access precedes the policy", s.writes)
	}
	policy := s.objects[networkPath]
	spec := policy["spec"].(map[string]any)
	check := func(direction, side string, index int, namespace, pod, protocol string, port float64) {
		rule := spec[direction].([]any)[index].(map[string]any)
		peers := rule[side].([]any)
		peer := peers[0].(map[string]any)
		ports := rule["ports"].([]any)
		if len(peers) != 1 || len(ports) != 1 || kube.String(peer, "namespaceSelector", "matchLabels", "kubernetes.io/metadata.name") != namespace || kube.String(peer, "podSelector", "matchLabels", "app") != pod || kube.String(ports[0].(map[string]any), "protocol") != protocol || ports[0].(map[string]any)["port"] != port {
			t.Fatal("incorrect network permission", rule)
		}
	}
	check("ingress", "from", 0, "control", "controller", "TCP", 8080)
	check("egress", "to", 0, networkName, "database", "TCP", 5432)
	check("egress", "to", 1, "cluster-dns", "dns", "UDP", 53)
	uid := kube.String(policy, "metadata", "uid")
	count := len(s.writes)
	s.mu.Unlock()
	restarted, err := New(a.client, "control")
	if err != nil {
		t.Fatal(err)
	}
	restarted.config.Profiles[0] = a.config.Profiles[0]
	if err = restarted.Ensure(ctx, "tenant", networkName, "owner-1"); err != nil {
		t.Fatal(err)
	}
	if err = restarted.RequireNamespace(ctx, "tenant", networkName, "owner-1"); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	if len(s.writes) != count || kube.String(s.objects[networkPath], "metadata", "uid") != uid {
		t.Fatal("restart rewrote the policy")
	}
	s.mu.Unlock()
	restarted.config.Profiles[0].NetworkPeers = selectedNetworkPeers()
	restarted.config.Profiles[0].NetworkPeers[0].Port = 8081
	if err = restarted.RequireNamespace(ctx, "tenant", networkName, "owner-1"); err == nil {
		t.Fatal("old policy permits work under a changed declaration")
	}
	if err = restarted.Ensure(ctx, "tenant", networkName, "owner-1"); !errors.Is(err, ErrPending) {
		t.Fatal("declaration update requires a fresh observation", err)
	}
	if err = restarted.Ensure(ctx, "tenant", networkName, "owner-1"); err != nil {
		t.Fatal(err)
	}
	if err = restarted.RequireNamespace(ctx, "tenant", networkName, "owner-1"); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	if len(s.writes) != count+1 || s.writes[count] != "PATCH "+networkPath || kube.String(s.objects[networkPath], "metadata", "uid") != uid || kube.String(s.objects[networkPath], "metadata", "resourceVersion") != "2" {
		t.Fatal("policy update lost identity or repeated a write", s.writes)
	}
	s.mu.Unlock()
	// Removal must delete prior rules, not leave them through merge semantics.
	restarted.config.Profiles[0].NetworkPeers = nil
	if err = restarted.Ensure(ctx, "tenant", networkName, "owner-1"); !errors.Is(err, ErrPending) {
		t.Fatal(err)
	}
	if err = restarted.Ensure(ctx, "tenant", networkName, "owner-1"); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	spec = s.objects[networkPath]["spec"].(map[string]any)
	if spec["ingress"] != nil || spec["egress"] != nil || len(s.writes) != count+2 {
		t.Fatal("retired peers remain", spec, s.writes)
	}
}

func TestAllocationNetworkPeersRejectWidening(t *testing.T) {
	cases := map[string]func(map[string]any){
		"allow all":          func(rule map[string]any) { delete(rule, "to") },
		"any pod":            func(rule map[string]any) { delete(rule["to"].([]any)[0].(map[string]any), "podSelector") },
		"any namespace":      func(rule map[string]any) { delete(rule["to"].([]any)[0].(map[string]any), "namespaceSelector") },
		"empty pod selector": func(rule map[string]any) { rule["to"].([]any)[0].(map[string]any)["podSelector"] = map[string]any{} },
		"other namespace": func(rule map[string]any) {
			rule["to"].([]any)[0].(map[string]any)["namespaceSelector"] = map[string]any{"matchLabels": map[string]any{"kubernetes.io/metadata.name": "tenant-87654321"}}
		},
		"selector OR": func(rule map[string]any) {
			peer := rule["to"].([]any)[0].(map[string]any)
			pod := peer["podSelector"]
			delete(peer, "podSelector")
			rule["to"] = []any{peer, map[string]any{"podSelector": pod}}
		},
		"CIDR": func(rule map[string]any) {
			rule["to"].([]any)[0].(map[string]any)["ipBlock"] = map[string]any{"cidr": "0.0.0.0/0"}
		},
		"all ports":      func(rule map[string]any) { delete(rule, "ports") },
		"other port":     func(rule map[string]any) { rule["ports"].([]any)[0].(map[string]any)["port"] = 5433 },
		"port range":     func(rule map[string]any) { rule["ports"].([]any)[0].(map[string]any)["endPort"] = 65535 },
		"named port":     func(rule map[string]any) { rule["ports"].([]any)[0].(map[string]any)["port"] = "postgres" },
		"other protocol": func(rule map[string]any) { rule["ports"].([]any)[0].(map[string]any)["protocol"] = "UDP" },
		"extra port": func(rule map[string]any) {
			rule["ports"] = append(rule["ports"].([]any), map[string]any{"port": 80, "protocol": "TCP"})
		},
		"unknown field": func(rule map[string]any) { rule["unknown"] = true },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			a, s := fixture(t)
			a.config.Profiles[0].NetworkIsolation = true
			a.config.Profiles[0].NetworkPeers = selectedNetworkPeers()
			s.mutateNetwork = func(o kube.Object) { mutate(o["spec"].(map[string]any)["egress"].([]any)[0].(map[string]any)) }
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := a.Ensure(ctx, "tenant", networkName, "owner-1"); err == nil {
				t.Fatal("changed policy granted access")
			}
			if err := a.RequireNamespace(ctx, "tenant", networkName, "owner-1"); err == nil {
				t.Fatal("changed policy permits work")
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if len(s.writes) != 3 {
				t.Fatal("changed policy caused binding writes", s.writes)
			}
		})
	}
}

func TestAllocationNetworkUpdateConflict(t *testing.T) {
	for _, code := range []int{403, 409, 500} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			a, s := fixture(t)
			a.config.Profiles[0].NetworkIsolation = true
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := a.Ensure(ctx, "tenant", networkName, "owner-1"); err != nil {
				t.Fatal(err)
			}
			a.config.Profiles[0].NetworkPeers = selectedNetworkPeers()
			calls := 0
			s.networkPatch = func(old, patch kube.Object) int {
				calls++
				if kube.String(patch, "metadata", "uid") != kube.String(old, "metadata", "uid") || kube.String(patch, "metadata", "resourceVersion") != kube.String(old, "metadata", "resourceVersion") {
					t.Error("patch lost observed identity")
				}
				return code
			}
			if err := a.Ensure(ctx, "tenant", networkName, "owner-1"); err == nil || errors.Is(err, ErrPending) {
				t.Fatal("failed patch reported progress", err)
			}
			if calls != 1 {
				t.Fatal("patch was retried without a new observation", calls)
			}
			if err := a.RequireNamespace(ctx, "tenant", networkName, "owner-1"); err == nil {
				t.Fatal("failed patch permitted work")
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if len(s.writes) != 7 || kube.String(s.objects[networkPath], "metadata", "resourceVersion") != "1" {
				t.Fatal("failed patch caused another write", s.writes)
			}
		})
	}
}

func TestAllocationNetworkLegacySeal(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(fmt.Sprint(changed), func(t *testing.T) {
			a, s := fixture(t)
			a.config.Profiles[0].NetworkIsolation = true
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := a.Ensure(ctx, "tenant", networkName, "owner-1"); err != nil {
				t.Fatal(err)
			}
			s.mu.Lock()
			delete(s.objects[networkPath]["metadata"].(map[string]any), "annotations")
			s.mu.Unlock()
			if changed {
				a.config.Profiles[0].NetworkPeers = selectedNetworkPeers()
			}
			err := a.Ensure(ctx, "tenant", networkName, "owner-1")
			if changed {
				if err == nil || errors.Is(err, ErrPending) {
					t.Fatal("changed legacy policy was adopted", err)
				}
				s.mu.Lock()
				defer s.mu.Unlock()
				if len(s.writes) != 6 {
					t.Fatal("changed legacy policy caused a write")
				}
			} else {
				if !errors.Is(err, ErrPending) {
					t.Fatal("matching legacy policy was not sealed", err)
				}
				if err = a.Ensure(ctx, "tenant", networkName, "owner-1"); err != nil {
					t.Fatal(err)
				}
				s.mu.Lock()
				defer s.mu.Unlock()
				if len(s.writes) != 7 || kube.String(s.objects[networkPath], "metadata", "annotations", networkHashAnnotation) == "" {
					t.Fatal("legacy seal missing or repeated")
				}
			}
		})
	}
}

func TestAllocationNetworkHashRejectsChanges(t *testing.T) {
	for _, hash := range []any{"", strings.Repeat("0", 64), true, map[string]any{"value": "invalid"}} {
		t.Run(fmt.Sprint(hash), func(t *testing.T) {
			a, s := fixture(t)
			a.config.Profiles[0].NetworkIsolation = true
			s.mutateNetwork = func(o kube.Object) {
				o["metadata"].(map[string]any)["annotations"].(map[string]any)[networkHashAnnotation] = hash
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := a.Ensure(ctx, "tenant", networkName, "owner-1"); err == nil {
				t.Fatal("changed seal was accepted")
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if len(s.writes) != 3 {
				t.Fatal("changed seal caused a binding write")
			}
		})
	}
}

func TestAllocationNetworkPeersRelatedOwner(t *testing.T) {
	a, s := fixture(t)
	p := &a.config.Profiles[0]
	p.NetworkIsolation = true
	p.NetworkPeers = []networkPeer{{Direction: "egress", Namespace: "profile", PeerProfile: "peer", PeerPrefix: "peer-", PodLabel: "app", PodValue: "service", Port: 8080, Protocol: "TCP"}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := a.Ensure(ctx, "tenant", networkName, "owner-1"); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	policy := s.objects[networkPath]
	target := policy["spec"].(map[string]any)["egress"].([]any)[0].(map[string]any)["to"].([]any)[0].(map[string]any)
	labels := target["namespaceSelector"].(map[string]any)["matchLabels"].(map[string]any)
	if len(labels) != 4 || labels["kubernetes.io/metadata.name"] != "peer-"+strings.TrimPrefix(networkName, p.Prefix) || labels["stego.dev/allocator"] != a.marker || labels["stego.dev/allocation-profile"] != "peer" || labels[p.OwnerLabel] != "owner-1" {
		t.Fatal("related network peer lost owner or installation identity", labels)
	}
	writes := len(s.writes)
	s.mu.Unlock()
	restarted, err := New(a.client, "control")
	if err != nil {
		t.Fatal(err)
	}
	restarted.config.Profiles[0] = *p
	if err = restarted.RequireNamespace(ctx, "tenant", networkName, "owner-1"); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	if len(s.writes) != writes {
		t.Fatal("read-only check wrote resources")
	}
	labels[p.OwnerLabel] = "owner-2"
	s.mu.Unlock()
	if err = restarted.RequireNamespace(ctx, "tenant", networkName, "owner-1"); err == nil {
		t.Fatal("network grant to another owner was accepted")
	}
}
