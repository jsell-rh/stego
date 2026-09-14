package allocation

import (
	"context"
	"errors"
	kube "example.com/widget/out/kubernetes"
	"testing"
)

func TestNamespaceUIDRequiresLiveAllocationIdentity(t *testing.T) {
	a, s := fixture(t)
	ctx := context.Background()
	name := "tenant-12345678"
	if uid, err := a.NamespaceUID(ctx, "tenant", name, "owner-1"); uid != "" || !errors.Is(err, ErrPending) {
		t.Fatal("missing namespace has an identity", uid, err)
	}
	if err := a.Ensure(ctx, "tenant", name, "owner-1"); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	ns := s.objects["/api/v1/namespaces/"+name]
	want := kube.String(ns, "metadata", "uid")
	writes := len(s.writes)
	s.mu.Unlock()
	if uid, err := a.NamespaceUID(ctx, "tenant", name, "owner-1"); err != nil || uid != want || uid == "" {
		t.Fatal("wrong allocation UID", uid, err)
	}
	if uid, err := a.NamespaceUID(ctx, "tenant", name, "owner-2"); uid != "" || err == nil {
		t.Fatal("foreign owner got allocation UID")
	}
	s.mu.Lock()
	meta := ns["metadata"].(map[string]any)
	meta["uid"] = "new-namespace"
	s.mu.Unlock()
	if uid, err := a.NamespaceUID(ctx, "tenant", name, "owner-1"); uid != "new-namespace" || err != nil {
		t.Fatal("namespace replacement has an old UID", uid, err)
	}
	s.mu.Lock()
	meta["deletionTimestamp"] = "2026-09-14T00:00:00Z"
	s.mu.Unlock()
	if uid, err := a.NamespaceUID(ctx, "tenant", name, "owner-1"); uid != "" || !errors.Is(err, ErrPending) {
		t.Fatal("deleting namespace is live", uid, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.writes) != writes {
		t.Fatal("identity read changed allocation")
	}
}
