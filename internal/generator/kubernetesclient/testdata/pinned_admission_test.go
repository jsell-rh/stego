package kubernetes

import (
	"encoding/json"
	"strings"
	"testing"
)

func pinnedFixture() []PinnedAdmissionRule {
	const uid = "11111111-2222-3333-4444-555555555555"
	return []PinnedAdmissionRule{
		{Name: "test-lifetime", Namespace: "test-ns", NamespaceUID: uid, APIVersion: "batch/v1", Kind: "Job", Resource: "jobs", ResourceName: "lifetime", TemplateNamespace: "templates", TemplateName: "lifetime", TemplateUID: uid, Mode: "job", DeadlineSeconds: 900},
		{Name: "test-worker", Namespace: "test-ns", NamespaceUID: uid, APIVersion: "apps/v1", Kind: "Deployment", Resource: "deployments", ResourceName: "worker", TemplateNamespace: "templates", TemplateName: "worker", TemplateUID: uid, Mode: "deployment", LifetimeJob: "lifetime"},
		{Name: "test-server", Namespace: "test-ns", NamespaceUID: uid, APIVersion: "example.test/v1", Kind: "Server", Resource: "servers", ResourceName: "server", TemplateNamespace: "templates", TemplateName: "server", TemplateUID: uid, Mode: "spec", LifetimeJob: "lifetime"},
	}
}

func TestPinnedAdmissionRejectsIncompleteAndConflictingAuthority(t *testing.T) {
	cases := []func([]PinnedAdmissionRule){
		func(r []PinnedAdmissionRule) { r[0].DeadlineSeconds = 0 },
		func(r []PinnedAdmissionRule) { r[0].DeadlineSeconds = 3601 },
		func(r []PinnedAdmissionRule) { r[0].Kind = "CronJob" },
		func(r []PinnedAdmissionRule) { r[1].LifetimeJob = "other" },
		func(r []PinnedAdmissionRule) { r[1].Namespace = "elsewhere" },
		func(r []PinnedAdmissionRule) { r[1].NamespaceUID = "" },
		func(r []PinnedAdmissionRule) { r[1].NamespaceUID = "99999999-2222-3333-4444-555555555555" },
		func(r []PinnedAdmissionRule) { r[1].TemplateUID = "" },
		func(r []PinnedAdmissionRule) {
			r[1].TemplateNamespace = r[1].Namespace
			r[1].TemplateName = r[1].ResourceName
		},
		func(r []PinnedAdmissionRule) { r[1].APIVersion = "apps/*" },
		func(r []PinnedAdmissionRule) { r[1].Resource = "deployments/scale" },
		func(r []PinnedAdmissionRule) { r[1].ResourceName = "worker' || true" },
		func(r []PinnedAdmissionRule) { r[2] = r[1]; r[2].Name = "duplicate-target" },
		func(r []PinnedAdmissionRule) { r[2].APIVersion = "rbac.authorization.k8s.io/v1" },
	}
	for i, change := range cases {
		rules := pinnedFixture()
		change(rules)
		if out, err := PinnedAdmissionPolicies("ci", "runner", rules); err == nil || out != nil {
			t.Fatalf("case %d returned partial authority", i)
		}
	}
	for _, actor := range []string{"", "runner*", "runner' || true"} {
		if out, err := PinnedAdmissionPolicies("ci", actor, pinnedFixture()); err == nil || out != nil {
			t.Fatal("invalid actor returned authority")
		}
	}
}

func TestPinnedAdmissionHasNoWriteGrantOrOptionalParameter(t *testing.T) {
	result, err := PinnedAdmissionPolicies("ci", "runner", pinnedFixture())
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 12 {
		t.Fatal("missing policy bindings")
	}
	for _, item := range result {
		raw, _ := json.Marshal(item)
		if strings.HasSuffix(item["metadata"].(Object)["name"].(string), ".subresources") {
			if item["kind"] == "ValidatingAdmissionPolicy" && !strings.Contains(string(raw), `"expression":"false"`) {
				t.Fatal("subresource write is not denied")
			}
			continue
		}
		switch item["kind"] {
		case "ValidatingAdmissionPolicy":
			for _, fragment := range []string{`"failurePolicy":"Fail"`, `system:serviceaccount:ci:runner`, `"scope":"Namespaced"`, `params.metadata.uid`, `stego.dev/pinned-namespace-uid`, `request.operation != 'UPDATE'`} {
				if !strings.Contains(string(raw), fragment) {
					t.Fatal("missing policy guard", fragment)
				}
			}
		case "ValidatingAdmissionPolicyBinding":
			if !strings.Contains(string(raw), `"parameterNotFoundAction":"Deny"`) || !strings.Contains(string(raw), `"validationActions":["Deny"]`) {
				t.Fatal("policy does not fail closed")
			}
		default:
			t.Fatal("policy renderer granted unrelated authority", item["kind"])
		}
	}
}
