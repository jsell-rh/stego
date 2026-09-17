package allocation

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	kube "example.com/widget/out/kubernetes"
)

func accountFixture(t *testing.T) (*Allocator, *api) {
	t.Helper()
	a, s := fixture(t)
	a.config.Profiles[0].ServiceAccounts = []string{"gateway", "reader"}
	return a, s
}

func TestAllocationServiceAccountIdentity(t *testing.T) {
	a, s := accountFixture(t)
	name, err := a.ServiceAccountName("tenant", "tenant-12345678", "owner-1", "gateway")
	if err != nil || name != "sa-5d51bf49cb135b20c33008b416b3ad5d-3fq6w77xbiorifsthf3yq75h2i" {
		t.Fatal("owner identity encoding changed", name, err)
	}
	again, err := a.ServiceAccountName("tenant", "tenant-87654321", "owner-1", "gateway")
	if err != nil || again != name {
		t.Fatal("namespace placement changed the owner identity")
	}
	for _, input := range [][2]string{{"owner-2", "gateway"}, {"owner-1", "reader"}} {
		next, err := a.ServiceAccountName("tenant", "tenant-12345678", input[0], input[1])
		if err != nil || next == name || len(next) > 63 || !dnsLabel.MatchString(next) {
			t.Fatal("distinct owner or alias shared an identity", err)
		}
	}
	for _, alias := range []string{"default", "missing", ""} {
		if _, err := a.ServiceAccountName("tenant", "tenant-12345678", "owner-1", alias); err == nil {
			t.Fatal("undeclared alias accepted")
		}
	}
	for _, mutate := range []func(){func() { a.namespace = "other" }, func() { a.config.Allocator = "other-allocator" }, func() { a.config.Profiles[0].OwnerLabel = "other.test/owner" }} {
		mutate()
		next, err := a.ServiceAccountName("tenant", "tenant-12345678", "owner-1", "gateway")
		if err != nil || next == name {
			t.Fatal("another installation or owner domain shared an identity", err)
		}
	}
	if s.requests != 0 {
		t.Fatal("name selection sent an API request")
	}
}

func TestAllocationServiceAccountsFollowLimits(t *testing.T) {
	a, s := accountFixture(t)
	ctx := context.Background()
	if err := a.Ensure(ctx, "tenant", "tenant-12345678", "owner-1"); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	quota, firstAccount, firstBinding := -1, -1, -1
	for i, write := range s.writes {
		if strings.HasSuffix(write, "/resourcequotas") {
			quota = i
		}
		if strings.HasSuffix(write, "/serviceaccounts") && firstAccount < 0 {
			firstAccount = i
		}
		if strings.HasSuffix(write, "/rolebindings") && firstBinding < 0 {
			firstBinding = i
		}
	}
	if quota < 0 || firstAccount <= quota || firstBinding <= firstAccount {
		t.Fatal("accounts or permissions preceded limits", s.writes)
	}
	p := a.config.Profiles[0]
	for _, alias := range p.ServiceAccounts {
		name := a.serviceAccountName(p, "owner-1", alias)
		ns := s.objects["/api/v1/namespaces/tenant-12345678"]
		sa := s.objects["/api/v1/namespaces/tenant-12345678/serviceaccounts/"+name]
		if kube.String(ns, "metadata", "annotations", serviceAccountAnnotation+alias) != name || sa["automountServiceAccountToken"] != false {
			t.Fatal("account is not bound to the owner with explicit token opt-in")
		}
	}
	collection, binding := a.bindingObject(p, "tenant-12345678", 1, kube.Owner{p.OwnerLabel: "owner-1"})
	actual := s.objects[collection+"/"+kube.String(binding, "metadata", "name")]
	if !kube.Contains(actual, binding) {
		t.Fatal("cluster binding lost its owner-specific account")
	}
}

func TestAllocationServiceAccountRejectsChangedSeal(t *testing.T) {
	a, s := accountFixture(t)
	ctx := context.Background()
	if err := a.Ensure(ctx, "tenant", "tenant-12345678", "owner-1"); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	annotations := kube.Nested(s.objects["/api/v1/namespaces/tenant-12345678"], "metadata", "annotations").(map[string]any)
	annotations[serviceAccountAnnotation+"gateway"] = "sa-" + a.marker + "-" + strings.Repeat("a", 26)
	before := len(s.writes)
	s.mu.Unlock()
	if err := a.Ensure(ctx, "tenant", "tenant-12345678", "owner-1"); err == nil {
		t.Fatal("changed identity was repaired or accepted")
	}
	if _, err := a.NamespaceUID(ctx, "tenant", "tenant-12345678", "owner-1"); err == nil {
		t.Fatal("changed identity passed the read-only proof")
	}
	if len(s.writes) != before {
		t.Fatal("changed identity caused a write")
	}
}

func TestAllocationServiceAccountNamespaceReuse(t *testing.T) {
	a, s := accountFixture(t)
	// Use a namespace role so this test isolates a retained namespace binding.
	// Cluster binding cleanup has a separate ownership check.
	a.config.Profiles[0].Bindings[1].Role = "data"
	ctx := context.Background()
	if err := a.Ensure(ctx, "tenant", "tenant-12345678", "owner-1"); err != nil {
		t.Fatal(err)
	}
	old, _ := a.ServiceAccountName("tenant", "tenant-12345678", "owner-1", "gateway")
	s.mu.Lock()
	// Model completed namespace removal. Keep a grant in a separate namespace.
	retained := kube.Object{"subjects": []any{kube.Object{"kind": "ServiceAccount", "namespace": "tenant-12345678", "name": old}}}
	for key := range s.objects {
		if key == "/api/v1/namespaces/tenant-12345678" || strings.Contains(key, "/namespaces/tenant-12345678/") {
			delete(s.objects, key)
		}
	}
	s.objects["/apis/rbac.authorization.k8s.io/v1/namespaces/retained/rolebindings/access"] = retained
	s.mu.Unlock()
	if err := a.Ensure(ctx, "tenant", "tenant-12345678", "owner-2"); err != nil {
		t.Fatal(err)
	}
	next, _ := a.ServiceAccountName("tenant", "tenant-12345678", "owner-2", "gateway")
	if old == next {
		t.Fatal("replacement owner inherited the prior account name")
	}
	if _, exists := s.objects["/api/v1/namespaces/tenant-12345678/serviceaccounts/"+old]; exists {
		t.Fatal("replacement recreated the prior account")
	}
	if s.objects["/api/v1/namespaces/tenant-12345678/serviceaccounts/"+next] == nil {
		t.Fatal("replacement account missing")
	}
	if retained["subjects"].([]any)[0].(kube.Object)["name"] != old {
		t.Fatal("unrelated grant was changed")
	}
}

func TestAllocationServiceAccountConcurrentOwners(t *testing.T) {
	a, s := accountFixture(t)
	a.config.Profiles[0].Bindings[1].Role = "data"
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, owner := range []string{"owner-1", "owner-2"} {
		wg.Add(1)
		go func(owner string) {
			defer wg.Done()
			<-start
			results <- a.Ensure(context.Background(), "tenant", "tenant-12345678", owner)
		}(owner)
	}
	close(start)
	wg.Wait()
	close(results)
	passed := 0
	for err := range results {
		if err == nil {
			passed++
		}
	}
	if passed != 1 {
		t.Fatal("allocation did not select exactly one owner", passed)
	}
	owner := kube.String(s.objects["/api/v1/namespaces/tenant-12345678"], "metadata", "labels", "example.test/owner")
	other := "owner-1"
	if owner == other {
		other = "owner-2"
	}
	for _, alias := range a.config.Profiles[0].ServiceAccounts {
		name := a.serviceAccountName(a.config.Profiles[0], other, alias)
		if s.objects["/api/v1/namespaces/tenant-12345678/serviceaccounts/"+name] != nil {
			t.Fatal("losing owner created an account")
		}
	}
}

func TestAllocationServiceAccountQuotaFailure(t *testing.T) {
	a, s := accountFixture(t)
	s.failQuota = true
	if err := a.Ensure(context.Background(), "tenant", "tenant-12345678", "owner-1"); err == nil {
		t.Fatal("failed quota accepted")
	}
	for _, write := range s.writes {
		if strings.HasSuffix(write, "/serviceaccounts") || strings.HasSuffix(write, "/rolebindings") || strings.HasSuffix(write, "/clusterrolebindings") {
			t.Fatal("quota failure granted access")
		}
	}
}

func TestAllocationServiceAccountReadiness(t *testing.T) {
	const namespace = "tenant-12345678"
	const namespacePath = "/api/v1/namespaces/" + namespace
	metadata := func(o kube.Object) map[string]any { return o["metadata"].(map[string]any) }
	labels := func(o kube.Object) map[string]any { return metadata(o)["labels"].(map[string]any) }
	tests := []struct {
		name           string
		change         func(kube.Object, kube.Object, *api, string)
		pending, valid bool
	}{
		{name: "ready", valid: true},
		{name: "missing_account", pending: true, change: func(_, _ kube.Object, s *api, path string) { delete(s.objects, path) }},
		{name: "missing_namespace", pending: true, change: func(_, _ kube.Object, s *api, _ string) { delete(s.objects, namespacePath) }},
		{name: "deleting_account", pending: true, change: func(account, _ kube.Object, _ *api, _ string) {
			metadata(account)["deletionTimestamp"] = "2026-09-17T00:00:00Z"
		}},
		{name: "deleting_namespace", pending: true, change: func(_, ns kube.Object, _ *api, _ string) { metadata(ns)["deletionTimestamp"] = "2026-09-17T00:00:00Z" }},
		{name: "foreign_account", change: func(account, _ kube.Object, _ *api, _ string) {
			labels(account)["example.test/owner"] = "another-owner"
		}},
		{name: "foreign_namespace", change: func(_, ns kube.Object, _ *api, _ string) { labels(ns)["example.test/owner"] = "another-owner" }},
		{name: "missing_uid", change: func(account, _ kube.Object, _ *api, _ string) { delete(metadata(account), "uid") }},
		{name: "missing_version", change: func(account, _ kube.Object, _ *api, _ string) { delete(metadata(account), "resourceVersion") }},
		{name: "wrong_name", change: func(account, _ kube.Object, _ *api, _ string) { metadata(account)["name"] = "another-account" }},
		{name: "wrong_namespace", change: func(account, _ kube.Object, _ *api, _ string) { metadata(account)["namespace"] = "tenant-87654321" }},
		{name: "missing_token_setting", change: func(account, _ kube.Object, _ *api, _ string) { delete(account, "automountServiceAccountToken") }},
		{name: "automatic_token_mount", change: func(account, _ kube.Object, _ *api, _ string) { account["automountServiceAccountToken"] = true }},
		{name: "invalid_token_setting", change: func(account, _ kube.Object, _ *api, _ string) { account["automountServiceAccountToken"] = "false" }},
		{name: "wrong_kind", change: func(account, _ kube.Object, _ *api, _ string) { account["kind"] = "ConfigMap" }},
		{name: "wrong_api_version", change: func(account, _ kube.Object, _ *api, _ string) { account["apiVersion"] = "other/v1" }},
		{name: "changed_namespace_seal", change: func(_, ns kube.Object, _ *api, _ string) {
			metadata(ns)["annotations"].(map[string]any)[serviceAccountAnnotation+"gateway"] = "another-account"
		}},
		{name: "read_denied", change: func(_, _ kube.Object, s *api, _ string) { s.failAccountRead = true }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			a, s := accountFixture(t)
			if err := a.Ensure(context.Background(), "tenant", namespace, "owner-1"); err != nil {
				t.Fatal(err)
			}
			expected, err := a.ServiceAccountName("tenant", namespace, "owner-1", "gateway")
			if err != nil {
				t.Fatal(err)
			}
			path := namespacePath + "/serviceaccounts/" + expected
			s.mu.Lock()
			beforeWrites, beforeRequests := len(s.writes), s.requests
			if test.change != nil {
				test.change(s.objects[path], s.objects[namespacePath], s, path)
			}
			s.mu.Unlock()
			name, err := a.RequireServiceAccount(context.Background(), "tenant", namespace, "owner-1", "gateway")
			if test.valid {
				if err != nil || name != expected {
					t.Fatal("ready account was not returned", err)
				}
			} else if err == nil || name != "" || errors.Is(err, ErrPending) != test.pending {
				t.Fatal("account readiness accepted an invalid identity or misclassified the result", err)
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if len(s.writes) != beforeWrites || s.requests-beforeRequests > 2 {
				t.Fatal("readiness wrote resources or exceeded two bounded reads")
			}
		})
	}
}

func TestAllocationServiceAccountReadinessRejectsInvalidInput(t *testing.T) {
	a, s := accountFixture(t)
	for _, input := range [][4]string{
		{"missing", "tenant-12345678", "owner-1", "gateway"},
		{"tenant", "another-12345678", "owner-1", "gateway"},
		{"tenant", "tenant-12345678", "", "gateway"},
		{"tenant", "tenant-12345678", "owner-1", "missing"},
	} {
		name, err := a.RequireServiceAccount(context.Background(), input[0], input[1], input[2], input[3])
		if err == nil || name != "" {
			t.Fatal("invalid account request was accepted")
		}
	}
	if s.requests != 0 {
		t.Fatal("invalid account request reached Kubernetes")
	}
}

func accountPeerFixture(t *testing.T, cycle bool) (*Allocator, *api) {
	t.Helper()
	a, s := accountFixture(t)
	p := a.config.Profiles[0]
	peer := p
	peer.Name = "jobs"
	peer.Prefix = "jobs-"
	peer.ServiceAccounts = []string{"runner"}
	peer.Bindings = []binding{{Role: "data", Namespace: "profile", SubjectProfile: p.Name, SubjectPrefix: p.Prefix, ServiceAccount: "gateway"}}
	a.config.Profiles = append(a.config.Profiles, peer)
	if cycle {
		a.config.Profiles[0].Bindings = append(a.config.Profiles[0].Bindings, binding{Role: "data", Namespace: "profile", SubjectProfile: peer.Name, SubjectPrefix: peer.Prefix, ServiceAccount: "runner"})
	}
	return a, s
}

func TestAllocationServiceAccountPeerGrant(t *testing.T) {
	a, s := accountPeerFixture(t, false)
	ctx := context.Background()
	if err := a.Ensure(ctx, "tenant", "tenant-12345678", "owner-1"); err != nil {
		t.Fatal(err)
	}
	if err := a.Ensure(ctx, "jobs", "jobs-12345678", "owner-1"); err != nil {
		t.Fatal(err)
	}
	peer := a.config.Profiles[1]
	owner := kube.Owner{peer.OwnerLabel: "owner-1"}
	collection, want := a.bindingObject(peer, "jobs-12345678", 0, owner)
	s.mu.Lock()
	defer s.mu.Unlock()
	got := s.objects[collection+"/"+kube.String(want, "metadata", "name")]
	subjects, ok := got["subjects"].([]any)
	if !ok || len(subjects) != 1 {
		t.Fatal("related permission has no exact subject")
	}
	subject, ok := subjects[0].(map[string]any)
	if !ok || subject["namespace"] != "tenant-12345678" || subject["name"] != a.serviceAccountName(peer, "owner-1", "gateway") {
		t.Fatal("grant selected a different namespace or owner")
	}
	imported := a.serviceAccountName(peer, "owner-1", "gateway")
	if _, ok := s.objects["/api/v1/namespaces/jobs-12345678/serviceaccounts/"+imported]; ok {
		t.Fatal("imported account was created in the destination")
	}
}

func TestAllocationServiceAccountPeerReadiness(t *testing.T) {
	cases := map[string]struct {
		pending bool
		change  func(*Allocator, *api)
	}{
		"missing namespace": {true, func(a *Allocator, s *api) { delete(s.objects, "/api/v1/namespaces/tenant-12345678") }},
		"missing account": {true, func(a *Allocator, s *api) {
			delete(s.objects, "/api/v1/namespaces/tenant-12345678/serviceaccounts/"+a.serviceAccountName(a.config.Profiles[0], "owner-1", "gateway"))
		}},
		"other owner": {false, func(a *Allocator, s *api) {
			s.objects["/api/v1/namespaces/tenant-12345678"]["metadata"].(map[string]any)["labels"].(map[string]any)[a.config.Profiles[0].OwnerLabel] = "owner-2"
		}},
		"terminating namespace": {true, func(a *Allocator, s *api) {
			s.objects["/api/v1/namespaces/tenant-12345678"]["metadata"].(map[string]any)["deletionTimestamp"] = "2026-09-17T00:00:00Z"
		}},
		"denied account read": {false, func(a *Allocator, s *api) {
			s.failAccountReadPrefix = "/api/v1/namespaces/tenant-12345678/serviceaccounts/"
		}},
		"wrong token setting": {false, func(a *Allocator, s *api) {
			s.objects["/api/v1/namespaces/tenant-12345678/serviceaccounts/"+a.serviceAccountName(a.config.Profiles[0], "owner-1", "gateway")]["automountServiceAccountToken"] = true
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			a, s := accountPeerFixture(t, false)
			a.config.Profiles[1].Bindings = append(a.config.Profiles[1].Bindings, binding{Role: "data", Namespace: "control", ServiceAccount: "widget-data"})
			ctx := context.Background()
			if err := a.Ensure(ctx, "tenant", "tenant-12345678", "owner-1"); err != nil {
				t.Fatal(err)
			}
			s.mu.Lock()
			tc.change(a, s)
			s.mu.Unlock()
			err := a.Ensure(ctx, "jobs", "jobs-12345678", "owner-1")
			if err == nil || errors.Is(err, ErrPending) != tc.pending {
				t.Fatal("invalid peer did not stop the grant", err)
			}
			peer := a.config.Profiles[1]
			collection, want := a.bindingObject(peer, "jobs-12345678", 0, kube.Owner{peer.OwnerLabel: "owner-1"})
			s.mu.Lock()
			defer s.mu.Unlock()
			if _, exists := s.objects[collection+"/"+kube.String(want, "metadata", "name")]; exists {
				t.Fatal("permission granted before peer readiness")
			}
			collection, want = a.bindingObject(peer, "jobs-12345678", 1, kube.Owner{peer.OwnerLabel: "owner-1"})
			if _, exists := s.objects[collection+"/"+kube.String(want, "metadata", "name")]; exists {
				t.Fatal("control worker grant preceded peer readiness")
			}

		})
	}
}

func TestAllocationServiceAccountPeerCycle(t *testing.T) {
	a, _ := accountPeerFixture(t, true)
	ctx := context.Background()
	if err := a.Ensure(ctx, "tenant", "tenant-12345678", "owner-1"); !errors.Is(err, ErrPending) {
		t.Fatal("missing peer must remain pending", err)
	}
	if err := a.Ensure(ctx, "jobs", "jobs-12345678", "owner-1"); err != nil {
		t.Fatal("second profile did not recover", err)
	}
	if err := a.Ensure(ctx, "tenant", "tenant-12345678", "owner-1"); err != nil {
		t.Fatal("cyclic account dependency did not recover", err)
	}
}

func TestAllocationServiceAccountPeerNamespaceRecovery(t *testing.T) {
	a, s := accountPeerFixture(t, false)
	ctx := context.Background()
	for _, p := range a.config.Profiles {
		if err := a.Ensure(ctx, p.Name, p.Prefix+"12345678", "owner-1"); err != nil {
			t.Fatal(err)
		}
	}
	s.mu.Lock()
	oldUID := kube.String(s.objects["/api/v1/namespaces/tenant-12345678"], "metadata", "uid")
	for path := range s.objects {
		if path == "/api/v1/namespaces/tenant-12345678" || strings.Contains(path, "/namespaces/tenant-12345678/") {
			delete(s.objects, path)
		}
	}
	s.mu.Unlock()
	if err := a.Ensure(ctx, "jobs", "jobs-12345678", "owner-1"); !errors.Is(err, ErrPending) {
		t.Fatal("absent source namespace was accepted", err)
	}
	if err := a.Ensure(ctx, "tenant", "tenant-12345678", "owner-1"); err != nil {
		t.Fatal(err)
	}
	if err := a.Ensure(ctx, "jobs", "jobs-12345678", "owner-1"); err != nil {
		t.Fatal("source recovery failed", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if kube.String(s.objects["/api/v1/namespaces/tenant-12345678"], "metadata", "uid") == oldUID {
		t.Fatal("fixture did not replace the source namespace")
	}
	peer := a.config.Profiles[1]
	collection, want := a.bindingObject(peer, "jobs-12345678", 0, kube.Owner{peer.OwnerLabel: "owner-1"})
	if !kube.Contains(s.objects[collection+"/"+kube.String(want, "metadata", "name")], want) {
		t.Fatal("source recovery changed the grant identity")
	}
}

func TestAllocationServiceAccountPeerRejectsChangedSeal(t *testing.T) {
	a, s := accountPeerFixture(t, false)
	ctx := context.Background()
	for _, p := range a.config.Profiles {
		if err := a.Ensure(ctx, p.Name, p.Prefix+"12345678", "owner-1"); err != nil {
			t.Fatal(err)
		}
	}
	s.mu.Lock()
	ns := s.objects["/api/v1/namespaces/jobs-12345678"]
	ns["metadata"].(map[string]any)["annotations"].(map[string]any)[serviceAccountAnnotation+"gateway"] = a.serviceAccountName(a.config.Profiles[1], "owner-2", "gateway")
	before := len(s.writes)
	s.mu.Unlock()
	if err := a.Ensure(ctx, "jobs", "jobs-12345678", "owner-1"); err == nil || errors.Is(err, ErrPending) {
		t.Fatal("changed imported identity was accepted", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.writes) != before {
		t.Fatal("changed imported identity caused a write")
	}
}
