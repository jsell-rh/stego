package kubernetesservice

import (
	_ "embed"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
)

func isolatedAllocationContext() gen.Context {
	c := allocationContext()
	p := c.ComponentConfig["allocation_profiles"].([]any)[0].(object)
	p["service_accounts"] = []any{"gateway"}
	p["pod_security"] = "isolated-runtime"
	p["pod_runtime_class"] = "stego-pods-test-runtime"
	p["pod_service_account"] = "gateway"
	p["network_isolation"] = true
	p["pod_annotations"] = []any{"example.test/workload"}
	p["pod_capability_grants"] = []any{object{"name": "network-helper", "image": "registry.invalid/stego/helper@sha256:" + strings.Repeat("b", 64), "capabilities": []any{"NET_ADMIN", "NET_RAW"}}}
	return c
}

func TestAllocationIsolatedValidation(t *testing.T) {
	cases := map[string]func(object){
		"unknown mode":               func(p object) { p["pod_security"] = "privileged" },
		"null mode":                  func(p object) { p["pod_security"] = nil },
		"empty mode":                 func(p object) { p["pod_security"] = "" },
		"boolean mode":               func(p object) { p["pod_security"] = true },
		"missing runtime":            func(p object) { delete(p, "pod_runtime_class") },
		"missing account":            func(p object) { delete(p, "pod_service_account") },
		"missing network isolation":  func(p object) { delete(p, "network_isolation") },
		"disabled network isolation": func(p object) { p["network_isolation"] = false },
		"restricted capabilities":    func(p object) { p["pod_security"] = "restricted" },
		"empty grants":               func(p object) { p["pod_capability_grants"] = []any{} },
		"null grants":                func(p object) { p["pod_capability_grants"] = nil },
		"too many grants":            func(p object) { p["pod_capability_grants"] = make([]any, 9) },
		"unknown grant field":        func(p object) { p["pod_capability_grants"].([]any)[0].(object)["other"] = true },
		"unsealed image":             func(p object) { p["pod_capability_grants"].([]any)[0].(object)["image"] = "helper:latest" },
		"template image":             func(p object) { p["pod_capability_grants"].([]any)[0].(object)["image"] = "{{.Image}}" },
		"wrong image type":           func(p object) { p["pod_capability_grants"].([]any)[0].(object)["image"] = true },
		"duplicate container": func(p object) {
			p["pod_capability_grants"] = append(p["pod_capability_grants"].([]any), p["pod_capability_grants"].([]any)[0])
		},
		"empty caps":  func(p object) { p["pod_capability_grants"].([]any)[0].(object)["capabilities"] = []any{} },
		"null caps":   func(p object) { p["pod_capability_grants"].([]any)[0].(object)["capabilities"] = nil },
		"unknown cap": func(p object) { p["pod_capability_grants"].([]any)[0].(object)["capabilities"] = []any{"NET_ADMN"} },
		"combined cap": func(p object) {
			p["pod_capability_grants"].([]any)[0].(object)["capabilities"] = []any{"NET_ADMIN NET_RAW"}
		},
		"wildcard cap": func(p object) { p["pod_capability_grants"].([]any)[0].(object)["capabilities"] = []any{"ALL"} },
		"duplicate cap": func(p object) {
			p["pod_capability_grants"].([]any)[0].(object)["capabilities"] = []any{"NET_ADMIN", "NET_ADMIN"}
		},
		"null annotation":            func(p object) { p["pod_annotations"] = nil },
		"empty annotation":           func(p object) { p["pod_annotations"] = []any{""} },
		"runtime annotation":         func(p object) { p["pod_annotations"] = []any{"io.katacontainers.config.hypervisor.shared_fs"} },
		"scc annotation":             func(p object) { p["pod_annotations"] = []any{"openshift.io/scc"} },
		"legacy security annotation": func(p object) { p["pod_annotations"] = []any{"container.apparmor.security.beta.kubernetes.io/helper"} },
		"duplicate annotation":       func(p object) { p["pod_annotations"] = []any{"example.test/workload", "example.test/workload"} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := isolatedAllocationContext()
			mutate(c.ComponentConfig["allocation_profiles"].([]any)[0].(object))
			files, wiring, err := new(Generator).Generate(c)
			if err == nil || files != nil || wiring != nil {
				t.Fatal("invalid isolated profile emitted output")
			}
		})
	}
	// An explicit default must not change the generated runtime or permissions.
	c := allocationContext()
	before, err := allocationConfig(c)
	if err != nil {
		t.Fatal(err)
	}
	c.ComponentConfig["allocation_profiles"].([]any)[0].(object)["pod_security"] = "restricted"
	after, err := allocationConfig(c)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("explicit restricted mode changed the default", err)
	}
}

func TestAllocationIsolatedPolicy(t *testing.T) {
	c := isolatedAllocationContext()
	config, err := allocationConfig(c)
	if err != nil {
		t.Fatal(err)
	}
	items, err := allocationObjects(config)
	if err != nil {
		t.Fatal(err)
	}
	policies := 0
	for _, raw := range items {
		item := raw.(object)
		if item["kind"] == "ValidatingAdmissionPolicy" {
			policies++
			if item["spec"].(object)["failurePolicy"] != "Fail" {
				t.Fatal("policy fails open")
			}
		}
		if item["kind"] == "ClusterRole" {
			for _, raw := range item["rules"].([]any) {
				rule := raw.(object)
				if resources, ok := rule["resources"].([]string); ok {
					for _, resource := range resources {
						if resource == "validatingadmissionpolicies" || resource == "runtimeclasses" {
							t.Fatal("allocator can change its runtime or policy")
						}
					}
				}
			}
		}
	}
	if policies != 8 {
		t.Fatal("missing guards", policies)
	}
	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"PodSecurity":"isolated-runtime"`) || strings.Contains(string(encoded), "NET_ADMIN") {
		t.Fatal("worker must receive the security mode but cannot interpret capability grants")
	}
}

func TestAllocationIsolatedManifests(t *testing.T) {
	t.Setenv("STEGO_ALLOCATION_SERVICE_ACCOUNTS", "1")
	c := isolatedAllocationContext()
	p := c.ComponentConfig["allocation_profiles"].([]any)[0].(object)
	p["bindings"] = append(p["bindings"].([]any), object{"external_role": "system:openshift:scc:privileged", "service_account": "gateway", "namespace": "allocated"})
	testAllocationManifests(t, c)
}

//go:embed testdata/allocation_isolated_test.go
var allocationIsolatedTests []byte

func TestGeneratedIsolatedAllocationRuntime(t *testing.T) {
	testGeneratedAllocationRuntime(t, "^TestIsolatedAllocation")
}
