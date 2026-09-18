package allocation

import (
	"context"
	kube "example.com/widget/out/kubernetes"
	"testing"
)

func TestIsolatedAllocationModeAndRestart(t *testing.T) {
	a, s := fixture(t)
	ctx := context.Background()
	name := "tenant-12345678"
	if err := a.Ensure(ctx, "tenant", name, "owner-1"); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	ns := s.objects["/api/v1/namespaces/"+name]
	level := kube.String(ns, "metadata", "labels", "pod-security.kubernetes.io/enforce")
	s.mu.Unlock()
	if level != "privileged" {
		t.Fatal("isolated allocation has the wrong Pod security level", level)
	}
	restarted, err := New(a.client, a.namespace)
	if err != nil {
		t.Fatal(err)
	}
	if err = restarted.RequireNamespace(ctx, "tenant", name, "owner-1"); err != nil {
		t.Fatal("valid allocation did not survive restart", err)
	}
	s.mu.Lock()
	ns["metadata"].(map[string]any)["labels"].(map[string]any)["pod-security.kubernetes.io/enforce"] = "restricted"
	writes := len(s.writes)
	s.mu.Unlock()
	if err = restarted.RequireNamespace(ctx, "tenant", name, "owner-1"); err == nil {
		t.Fatal("changed namespace security was accepted")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.writes) != writes {
		t.Fatal("read-only readiness check changed the namespace")
	}
}
