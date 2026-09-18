package kubernetesservice

import (
	"encoding/json"
	"strings"
	"testing"

	"cel.dev/cel-go/cel"
)

func TestAllocationNetworkMetadataValidation(t *testing.T) {
	for name, change := range map[string]func(object){
		"unknown provider": func(p object) { p["pod_network_provider"] = "other" },
		"empty provider":   func(p object) { p["pod_network_provider"] = "" },
		"null provider":    func(p object) { p["pod_network_provider"] = nil },
		"boolean provider": func(p object) { p["pod_network_provider"] = true },
		"restricted profile": func(p object) {
			p["pod_security"] = "restricted"
			delete(p, "pod_capability_grants")
			delete(p, "pod_annotations")
		},
		"application OVN key":         func(p object) { p["pod_annotations"] = []any{ovnPodNetworks} },
		"application Multus status":   func(p object) { p["pod_annotations"] = []any{multusNetworkStatus} },
		"application network request": func(p object) { p["pod_annotations"] = []any{"k8s.v1.cni.cncf.io/networks"} },
	} {
		t.Run(name, func(t *testing.T) {
			c := isolatedAllocationContext()
			p := c.ComponentConfig["allocation_profiles"].([]any)[0].(object)
			p["pod_network_provider"] = "openshift-ovn-node-identity"
			change(p)
			files, wiring, err := new(Generator).Generate(c)
			if err == nil || files != nil || wiring != nil {
				t.Fatal("invalid network metadata configuration emitted output")
			}
		})
	}
	c := isolatedAllocationContext()
	c.ComponentConfig["allocation_profiles"].([]any)[0].(object)["pod_network_provider"] = "openshift-ovn-node-identity"
	config, err := allocationConfig(c)
	if err != nil {
		t.Fatal(err)
	}
	if config.Profiles[0].PodNetworkProvider != "openshift-ovn-node-identity" {
		t.Fatal("provider was lost")
	}
	encoded, err := json.Marshal(config)
	if err != nil || strings.Contains(string(encoded), "openshift-ovn-node-identity") {
		t.Fatal("worker received network policy authority", err)
	}
}

func TestAllocationNetworkMetadataStatusPolicy(t *testing.T) {
	c := isolatedAllocationContext()
	config, err := allocationConfig(c)
	if err != nil {
		t.Fatal(err)
	}
	p := config.Profiles[0]
	items := allocationPodObjects(config, p, "true")
	raw, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"pods/status"`) {
		t.Fatal("Pod status bypasses the policy")
	}
	// Network output is rejected unless the operator selects its provider.
	if strings.Contains(string(raw), ovnPodNetworks) || strings.Contains(string(raw), multusNetworkStatus) {
		t.Fatal("default profile granted network annotation keys")
	}
	p.PodNetworkProvider = "openshift-ovn-node-identity"
	items = allocationPodObjects(config, p, "true")
	raw, err = json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{ovnPodNetworks, multusNetworkStatus, "system:ovn-node:", "system:multus:", "Isolated containers cannot request privileged mode", "Added capabilities require"} {
		if !strings.Contains(string(raw), required) {
			t.Fatal("missing policy guard", required)
		}
	}
}

func TestAllocationNetworkMetadataCEL(t *testing.T) {
	env, err := cel.NewEnv(cel.Variable("object", cel.DynType), cel.Variable("oldObject", cel.DynType), cel.Variable("request", cel.DynType))
	if err != nil {
		t.Fatal(err)
	}
	for _, provider := range []struct{ key, prefix, group string }{{ovnPodNetworks, "system:ovn-node:", "system:ovn-nodes"}, {multusNetworkStatus, "system:multus:", "system:multus"}} {
		t.Run(provider.key, func(t *testing.T) {
			ast, issues := env.Compile(allocationNetworkMetadataCEL(provider.key, provider.prefix, provider.group))
			if issues.Err() != nil {
				t.Fatal(issues.Err())
			}
			program, err := env.Program(ast, cel.CostLimit(10000))
			if err != nil {
				t.Fatal(err)
			}
			pod := func(value string) object {
				annotations := object{}
				if value != "" {
					annotations[provider.key] = value
				}
				return object{"metadata": object{"annotations": annotations}, "spec": object{"nodeName": "node-one"}}
			}
			cases := map[string]struct {
				allowed bool
				change  func(object)
			}{
				"assigned node adds":     {true, func(object) {}},
				"assigned node replaces": {true, func(v object) { v["oldObject"] = pod("old") }},
				"assigned node removes":  {true, func(v object) { v["oldObject"] = pod("old"); v["object"] = pod("") }},
				"workload adds": {false, func(v object) {
					v["request"].(object)["userInfo"].(object)["username"] = "system:serviceaccount:tenant:workload"
				}},
				"other node": {false, func(v object) { v["request"].(object)["userInfo"].(object)["username"] = provider.prefix + "node-two" }},
				"prefix spoof": {false, func(v object) {
					v["request"].(object)["userInfo"].(object)["username"] = provider.prefix + "node-one:extra"
				}},
				"wrong group":           {false, func(v object) { v["request"].(object)["userInfo"].(object)["groups"] = []any{"system:authenticated"} }},
				"missing group":         {false, func(v object) { delete(v["request"].(object)["userInfo"].(object), "groups") }},
				"different subresource": {false, func(v object) { v["request"].(object)["subResource"] = "ephemeralcontainers" }},
				"main resource write":   {false, func(v object) { v["request"].(object)["subResource"] = "" }},
				"create injection":      {false, func(v object) { v["oldObject"] = nil; v["request"].(object)["operation"] = "CREATE" }},
				"new assigned node":     {false, func(v object) { v["object"].(object)["spec"].(object)["nodeName"] = "node-two" }},
				"missing assignment":    {false, func(v object) { delete(v["oldObject"].(object)["spec"].(object), "nodeName") }},
				"empty assignment":      {false, func(v object) { v["oldObject"].(object)["spec"].(object)["nodeName"] = "" }},
				"workload preserves": {true, func(v object) {
					v["oldObject"] = pod("new")
					v["request"].(object)["userInfo"] = object{"username": "workload", "groups": []any{}}
				}},
				"workload changes": {false, func(v object) {
					v["oldObject"] = pod("old")
					v["request"].(object)["userInfo"] = object{"username": "workload", "groups": []any{}}
				}},
				"workload removes": {false, func(v object) {
					v["oldObject"] = pod("old")
					v["object"] = pod("")
					v["request"].(object)["userInfo"] = object{"username": "workload", "groups": []any{}}
				}},
				"create without metadata": {true, func(v object) {
					v["oldObject"] = nil
					v["object"] = pod("")
					v["request"].(object)["operation"] = "CREATE"
				}},
			}
			for name, test := range cases {
				t.Run(name, func(t *testing.T) {
					values := object{"object": pod("new"), "oldObject": pod(""), "request": object{"operation": "UPDATE", "subResource": "status", "userInfo": object{"username": provider.prefix + "node-one", "groups": []any{provider.group, "system:authenticated"}}}}
					test.change(values)
					result, _, err := program.Eval(values)
					if test.allowed {
						if err != nil || result.Value() != true {
							t.Fatal("valid network update denied", result, err)
						}
					} else if err == nil && result.Value() != false {
						t.Fatal("untrusted network update permitted", result)
					}
				})
			}
		})
	}
}

func TestAllocationNetworkMetadataManifests(t *testing.T) {
	t.Setenv("STEGO_ALLOCATION_SERVICE_ACCOUNTS", "1")
	c := isolatedAllocationContext()
	p := c.ComponentConfig["allocation_profiles"].([]any)[0].(object)
	p["pod_network_provider"] = "openshift-ovn-node-identity"
	p["pod_annotations"] = []any{"example.test/workload", "stego.test/create-intent"}
	p["bindings"] = append(p["bindings"].([]any), object{"external_role": "system:openshift:scc:privileged", "service_account": "gateway", "namespace": "allocated"})
	testAllocationManifests(t, c)
}
