package kubernetesservice

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestAllocationPodValidation(t *testing.T) {
	for name, value := range map[string]any{
		"empty": "", "null": nil, "number": 1, "space": "kata runtime", "path": "../kata",
		"template": "{{.Namespace}}", "uppercase": "Kata", "trailing_dot": "kata.",
		"empty_label": "kata..test", "long_label": strings.Repeat("a", 64),
		"long_name": strings.Repeat(strings.Repeat("a", 63)+".", 4) + "a",
	} {
		t.Run("runtime/"+name, func(t *testing.T) {
			c := allocationContext()
			c.ComponentConfig["allocation_profiles"].([]any)[0].(object)["pod_runtime_class"] = value
			files, wiring, err := new(Generator).Generate(c)
			if err == nil || files != nil || wiring != nil {
				t.Fatal("invalid runtime returned generated output")
			}
		})
	}
	for name, value := range map[string]any{"empty": "", "null": nil, "number": 1, "default": "default", "undeclared": "other", "literal": "sa-123", "template": "{{.Namespace}}"} {
		t.Run("account/"+name, func(t *testing.T) {
			c := allocationContext()
			p := c.ComponentConfig["allocation_profiles"].([]any)[0].(object)
			p["service_accounts"] = []any{"gateway"}
			p["pod_service_account"] = value
			files, wiring, err := new(Generator).Generate(c)
			if err == nil || files != nil || wiring != nil {
				t.Fatal("invalid account returned generated output")
			}
		})
	}
	for _, runtime := range []string{"kata", "kata.test", strings.Repeat("a", 63)} {
		c := allocationContext()
		p := c.ComponentConfig["allocation_profiles"].([]any)[0].(object)
		p["pod_runtime_class"] = runtime
		p["service_accounts"] = []any{"gateway"}
		p["pod_service_account"] = "gateway"
		config, err := allocationConfig(c)
		if err != nil || config.Profiles[0].PodRuntimeClass != runtime || config.Profiles[0].PodServiceAccount != "gateway" {
			t.Fatal("valid Pod configuration was lost", err)
		}
	}
}

func TestAllocationPodPolicy(t *testing.T) {
	c := allocationContext()
	p := c.ComponentConfig["allocation_profiles"].([]any)[0].(object)
	p["service_accounts"] = []any{"gateway"}
	before, err := allocationConfig(c)
	if err != nil {
		t.Fatal(err)
	}
	old, err := allocationObjects(before)
	if err != nil {
		t.Fatal(err)
	}
	p["pod_runtime_class"] = "kata.test"
	p["pod_service_account"] = "gateway"
	config, err := allocationConfig(c)
	if err != nil {
		t.Fatal(err)
	}
	items, err := allocationObjects(config)
	if err != nil {
		t.Fatal(err)
	}
	name := "{{.Namespace}}.widget-queue.pods.tenant"
	unchanged := []any{}
	var policy object
	for _, raw := range items {
		item := raw.(object)
		if item["metadata"].(object)["name"] != name {
			unchanged = append(unchanged, item)
			continue
		}
		if item["kind"] == "ValidatingAdmissionPolicy" {
			policy = item
		} else if item["kind"] != "ValidatingAdmissionPolicyBinding" || !reflect.DeepEqual(item["spec"].(object)["validationActions"], []string{"Deny"}) {
			t.Fatal("Pod restriction added a grant or a non-blocking binding")
		}
	}
	if len(items) != len(old)+2 || !reflect.DeepEqual(old, unchanged) || policy == nil {
		t.Fatal("Pod restrictions changed existing allocation policies or permissions")
	}
	spec := policy["spec"].(object)
	constraints := spec["matchConstraints"].(object)
	if spec["failurePolicy"] != "Fail" || !reflect.DeepEqual(constraints["namespaceSelector"], object{"matchLabels": object{"stego.dev/allocator": allocationMarker(config), "stego.dev/allocation-profile": "tenant"}}) {
		t.Fatal("Pod policy must select one installation and fail closed")
	}
	rule := constraints["resourceRules"].([]any)[0].(object)
	if !reflect.DeepEqual(rule["operations"], []string{"CREATE", "UPDATE"}) || !reflect.DeepEqual(rule["resources"], []string{"pods", "pods/status", "pods/ephemeralcontainers"}) || rule["scope"] != "Namespaced" {
		t.Fatal("Pod policy must cover main, status, and ephemeral writes and retain deletion")
	}
	raw, _ := json.Marshal(spec["validations"])
	for _, text := range []string{"namespaceObject != null", "'restricted'", "stego.dev/service-account-gateway", "automountServiceAccountToken", "object.spec.runtimeClassName", "kata.test", "example.test/owner"} {
		if !strings.Contains(string(raw), text) {
			t.Fatal("missing admission restriction", text)
		}
	}
	// The worker does not interpret policy fields. Existing runtime output stays
	// identical; the installation manifest is the source of these restrictions.
	first, _ := json.Marshal(before)
	second, _ := json.Marshal(config)
	if string(first) != string(second) {
		t.Fatal("Pod restrictions changed allocator runtime configuration")
	}
}

func TestAllocationPodManifests(t *testing.T) {
	t.Setenv("STEGO_ALLOCATION_SERVICE_ACCOUNTS", "1")
	c := allocationContext()
	p := c.ComponentConfig["allocation_profiles"].([]any)[0].(object)
	p["pod_runtime_class"] = "stego-pods-test-runtime"
	p["pod_service_account"] = "gateway"
	testAllocationManifests(t, c)
}
