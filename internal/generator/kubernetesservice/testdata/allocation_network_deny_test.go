package allocation

import (
	"context"
	"errors"
	kube "example.com/widget/out/kubernetes"
	"strings"
	"testing"
	"time"
)

const networkName = "tenant-12345678"
const networkPath = "/apis/networking.k8s.io/v1/namespaces/" + networkName + "/networkpolicies/stego-allocation"

func TestAllocationNetworkLifecycle(t *testing.T) {
	a, s := fixture(t)
	a.config.Profiles[0].NetworkIsolation = true
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := a.Ensure(ctx, "tenant", networkName, "owner-1"); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	if len(s.writes) != 6 || s.writes[2] != "POST "+strings.TrimSuffix(networkPath, "/stego-allocation") {
		t.Fatal("network policy did not precede bindings", s.writes)
	}
	policy := s.objects[networkPath]
	uid := kube.String(policy, "metadata", "uid")
	count := len(s.writes)
	// The API can omit empty fields and can return policy types in either order.
	spec := policy["spec"].(map[string]any)
	spec["policyTypes"] = []any{"Egress", "Ingress"}
	spec["podSelector"] = map[string]any{"matchLabels": map[string]any{}, "matchExpressions": []any{}}
	spec["ingress"] = []any{}
	spec["egress"] = []any{}
	s.mu.Unlock()
	restarted, err := New(a.client, "control")
	if err != nil {
		t.Fatal(err)
	}
	restarted.config.Profiles[0].NetworkIsolation = true
	if err = restarted.Ensure(ctx, "tenant", networkName, "owner-1"); err != nil {
		t.Fatal(err)
	}
	if err = restarted.RequireNamespace(ctx, "tenant", networkName, "owner-1"); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	if len(s.writes) != count || kube.String(s.objects[networkPath], "metadata", "uid") != uid {
		t.Fatal("restart changed the policy")
	}
	delete(s.objects, networkPath)
	s.mu.Unlock()
	if err = restarted.RequireNamespace(ctx, "tenant", networkName, "owner-1"); !errors.Is(err, ErrPending) {
		t.Fatal("missing policy permits work", err)
	}
	s.mu.Lock()
	if len(s.writes) != count {
		t.Fatal("read-only check wrote a policy")
	}
	s.mu.Unlock()
	if err = restarted.Ensure(ctx, "tenant", networkName, "owner-1"); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	if len(s.writes) != count+1 {
		t.Fatal("policy recovery changed bindings")
	}
	s.mu.Unlock()
	// Removal of the option must not delete a policy from an existing namespace.
	restarted.config.Profiles[0].NetworkIsolation = false
	if err = restarted.Ensure(ctx, "tenant", networkName, "owner-1"); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.objects[networkPath]; !ok || len(s.writes) != count+1 {
		t.Fatal("option removal changed the policy")
	}
}

func TestAllocationNetworkRejectsInvalidPolicy(t *testing.T) {
	cases := map[string]func(kube.Object){
		"foreign owner": func(o kube.Object) {
			o["metadata"].(map[string]any)["labels"].(map[string]any)["example.test/owner"] = "other"
		},
		"missing UID":      func(o kube.Object) { delete(o["metadata"].(map[string]any), "uid") },
		"missing version":  func(o kube.Object) { delete(o["metadata"].(map[string]any), "resourceVersion") },
		"deleting":         func(o kube.Object) { o["metadata"].(map[string]any)["deletionTimestamp"] = "2026-09-15T00:00:00Z" },
		"wrong namespace":  func(o kube.Object) { o["metadata"].(map[string]any)["namespace"] = "other" },
		"wrong name":       func(o kube.Object) { o["metadata"].(map[string]any)["name"] = "other" },
		"missing selector": func(o kube.Object) { delete(o["spec"].(map[string]any), "podSelector") },
		"null selector":    func(o kube.Object) { o["spec"].(map[string]any)["podSelector"] = nil },
		"selected labels": func(o kube.Object) {
			o["spec"].(map[string]any)["podSelector"] = map[string]any{"matchLabels": map[string]any{"app": "other"}}
		},
		"selected expression": func(o kube.Object) {
			o["spec"].(map[string]any)["podSelector"] = map[string]any{"matchExpressions": []any{map[string]any{"key": "app", "operator": "Exists"}}}
		},
		"unknown selector":    func(o kube.Object) { o["spec"].(map[string]any)["podSelector"] = map[string]any{"unknown": true} },
		"ingress allowed":     func(o kube.Object) { o["spec"].(map[string]any)["ingress"] = []any{map[string]any{}} },
		"egress allowed":      func(o kube.Object) { o["spec"].(map[string]any)["egress"] = []any{map[string]any{}} },
		"missing types":       func(o kube.Object) { delete(o["spec"].(map[string]any), "policyTypes") },
		"one direction":       func(o kube.Object) { o["spec"].(map[string]any)["policyTypes"] = []any{"Ingress"} },
		"duplicate direction": func(o kube.Object) { o["spec"].(map[string]any)["policyTypes"] = []any{"Ingress", "Ingress"} },
		"unknown direction":   func(o kube.Object) { o["spec"].(map[string]any)["policyTypes"] = []any{"Ingress", "Other"} },
		"extra direction":     func(o kube.Object) { o["spec"].(map[string]any)["policyTypes"] = []any{"Ingress", "Egress", "Ingress"} },
		"malformed rule":      func(o kube.Object) { o["spec"].(map[string]any)["egress"] = map[string]any{} },
		"unknown spec":        func(o kube.Object) { o["spec"].(map[string]any)["unknown"] = true },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			a, s := fixture(t)
			a.config.Profiles[0].NetworkIsolation = true
			s.mutateNetwork = mutate
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := a.Ensure(ctx, "tenant", networkName, "owner-1"); err == nil {
				t.Fatal("invalid policy granted access")
			}
			if err := a.RequireNamespace(ctx, "tenant", networkName, "owner-1"); err == nil {
				t.Fatal("invalid policy permits work")
			}
			if err := a.Ensure(ctx, "tenant", networkName, "owner-1"); err == nil {
				t.Fatal("invalid stored policy granted access")
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if len(s.writes) != 3 {
				t.Fatal("invalid policy caused further writes", s.writes)
			}
		})
	}
}

func TestAllocationNetworkAPIFailure(t *testing.T) {
	for _, name := range []string{"create denied", "read denied", "read denied after create", "canceled"} {
		t.Run(name, func(t *testing.T) {
			a, s := fixture(t)
			a.config.Profiles[0].NetworkIsolation = true
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			switch name {
			case "create denied":
				s.failNetwork = true
			case "read denied":
				s.failNetworkRead = true
			case "read denied after create":
				s.mutateNetwork = func(kube.Object) { s.failNetworkRead = true }
			case "canceled":
				cancel()
			}
			if err := a.Ensure(ctx, "tenant", networkName, "owner-1"); err == nil {
				t.Fatal("failed policy check granted access")
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			for _, write := range s.writes {
				if strings.Contains(write, "rolebindings") {
					t.Fatal("failed policy check wrote a binding")
				}
			}
		})
	}
}
