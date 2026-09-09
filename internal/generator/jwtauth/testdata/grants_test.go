package auth

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func testGrant() Grant {
	return Grant{Issuer: "https://issuer.example", Subject: "worker", Resource: "Lease", Operation: "cleanup.worker", Target: "region-a"}
}

func TestGrantsMatchEveryAuthorityDimension(t *testing.T) {
	grant := testGrant()
	input := []Grant{grant}
	policy, err := NewGrantPolicy(input)
	if err != nil {
		t.Fatal(err)
	}
	identity := Identity{Issuer: grant.Issuer, UserID: grant.Subject}
	if !policy.Allows(identity, grant.Resource, grant.Operation, grant.Target) {
		t.Fatal("exact grant was denied")
	}
	input[0].Target = "region-b"
	for _, change := range []func(*Grant){
		func(g *Grant) { g.Issuer = "https://other.example" }, func(g *Grant) { g.Subject = "other" },
		func(g *Grant) { g.Resource = "Other" }, func(g *Grant) { g.Operation = "cleanup.identity" },
		func(g *Grant) { g.Target = "region-b" }, func(g *Grant) { g.Target = "" },
		func(g *Grant) { g.Target = "region-a/child" }, func(g *Grant) { g.Target = "REGION-A" },
	} {
		altered := grant
		change(&altered)
		caller := Identity{Issuer: altered.Issuer, UserID: altered.Subject, Role: "admin", Roles: []string{"admin"}}
		if policy.Allows(caller, altered.Resource, altered.Operation, altered.Target) {
			t.Fatal("grant crossed an authority boundary")
		}
	}
	if !policy.Allows(identity, grant.Resource, grant.Operation, grant.Target) {
		t.Fatal("caller mutation changed the policy")
	}
	for _, empty := range []*GrantPolicy{nil, {}} {
		if empty.Allows(identity, grant.Resource, grant.Operation, grant.Target) {
			t.Fatal("empty policy allowed a write")
		}
	}
	var group sync.WaitGroup
	for range 16 {
		group.Add(1)
		go func() {
			defer group.Done()
			for range 1000 {
				if !policy.Allows(identity, grant.Resource, grant.Operation, grant.Target) {
					t.Error("concurrent read changed authorization")
					return
				}
			}
		}()
	}
	group.Wait()
}

func TestEmptyAndOpaqueTargetsAreExact(t *testing.T) {
	grant := testGrant()
	grant.Target = ""
	policy, err := NewGrantPolicy([]Grant{grant})
	if err != nil {
		t.Fatal(err)
	}
	identity := Identity{Issuer: grant.Issuer, UserID: grant.Subject}
	if !policy.Allows(identity, grant.Resource, grant.Operation, "") || policy.Allows(identity, grant.Resource, grant.Operation, "region-a") {
		t.Fatal("empty scope acted as a wildcard")
	}
	grant.Target = "*"
	policy, err = NewGrantPolicy([]Grant{grant})
	if err != nil {
		t.Fatal(err)
	}
	if !policy.Allows(identity, grant.Resource, grant.Operation, "*") || policy.Allows(identity, grant.Resource, grant.Operation, "region-a") {
		t.Fatal("opaque target acted as a wildcard")
	}
}

func TestGrantDocumentsRejectAmbiguousConfiguration(t *testing.T) {
	encoded, _ := json.Marshal([]Grant{testGrant()})
	if _, err := ParseGrantPolicy(encoded); err != nil {
		t.Fatal(err)
	}
	if policy, err := ParseGrantPolicy([]byte(`[]`)); err != nil || policy.Allows(Identity{}, "Lease", "read", "") {
		t.Fatal("empty policy is invalid or allows access", err)
	}
	for _, raw := range []string{
		`null`, `{}`, `[null]`, `[] true`, string(encoded) + `[]`,
		strings.Replace(string(encoded), `"subject":"worker"`, `"subject":"worker","subject":"other"`, 1),
		strings.Replace(string(encoded), `"subject":"worker"`, `"subject":null`, 1),
		strings.Replace(string(encoded), `"subject":"worker"`, `"subject":123`, 1),
		strings.Replace(string(encoded), `"subject":"worker"`, `"subject":[]`, 1),
		strings.Replace(string(encoded), `"subject":"worker",`, ``, 1),
		strings.Replace(string(encoded), `"subject"`, `"Subject"`, 1),
		strings.Replace(string(encoded), `"subject":"worker"`, `"subject":"worker","extra":"ignored"`, 1),
		strings.Replace(string(encoded), `"worker"`, `"\ud800"`, 1),
		strings.Replace(string(encoded), `"worker"`, `" worker"`, 1),
		strings.Replace(string(encoded), `"worker"`, `"worker\u0000"`, 1),
		strings.Replace(string(encoded), `"Lease"`, `"*"`, 1),
		strings.Repeat(" ", 262145), string([]byte{0xff}),
	} {
		if policy, err := ParseGrantPolicy([]byte(raw)); err == nil || policy != nil {
			t.Fatal("ambiguous grant document was accepted")
		}
	}
}

func TestGrantConstructionEnforcesBoundsAndUniqueness(t *testing.T) {
	grant := testGrant()
	if _, err := NewGrantPolicy([]Grant{grant, grant}); err == nil {
		t.Fatal("repeated grant accepted")
	}
	if _, err := NewGrantPolicy(make([]Grant, 1025)); err == nil {
		t.Fatal("grant count limit ignored")
	}
	for _, change := range []func(*Grant){func(g *Grant) { g.Issuer = "" }, func(g *Grant) { g.Subject = "" }, func(g *Grant) { g.Target = strings.Repeat("x", 257) }, func(g *Grant) { g.Operation = "delete *" }, func(g *Grant) { g.Resource = "bad/resource" }} {
		altered := grant
		change(&altered)
		if _, err := NewGrantPolicy([]Grant{altered}); err == nil {
			t.Fatal("invalid grant accepted")
		}
	}
}

func BenchmarkGrantPolicy(b *testing.B) {
	for _, count := range []int{1, 1024} {
		b.Run(fmt.Sprintf("grants_%d", count), func(b *testing.B) {
			grants := make([]Grant, count)
			for i := range grants {
				grants[i] = testGrant()
				grants[i].Subject = fmt.Sprintf("worker-%d", i)
			}
			policy, err := NewGrantPolicy(grants)
			if err != nil {
				b.Fatal(err)
			}
			identity := Identity{Issuer: grants[0].Issuer, UserID: grants[0].Subject}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if !policy.Allows(identity, grants[0].Resource, grants[0].Operation, grants[0].Target) {
					b.Fatal("grant was denied")
				}
			}
		})
	}
}
