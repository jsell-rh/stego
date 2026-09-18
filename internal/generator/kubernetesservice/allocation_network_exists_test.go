package kubernetesservice

import (
	"context"
	"encoding/json"
	"testing"

	"cel.dev/cel-go/cel"
)

func existsPeer() object {
	return object{"direction": "ingress", "namespace": "allocated", "pod_exists_label": "example.test/workload", "port": 8080, "protocol": "TCP"}
}

func TestAllocationNetworkPeerExistsValidation(t *testing.T) {
	for name, change := range map[string]func(object){
		"null":           func(p object) { p["pod_exists_label"] = nil },
		"wrong type":     func(p object) { p["pod_exists_label"] = true },
		"empty":          func(p object) { p["pod_exists_label"] = "" },
		"wildcard":       func(p object) { p["pod_exists_label"] = "*" },
		"template":       func(p object) { p["pod_exists_label"] = "{{.Namespace}}" },
		"label too":      func(p object) { p["pod_label"] = "app" },
		"value too":      func(p object) { p["pod_value"] = "service" },
		"null label too": func(p object) { p["pod_label"] = nil },
		"null value too": func(p object) { p["pod_value"] = nil },
	} {
		t.Run(name, func(t *testing.T) {
			p := existsPeer()
			change(p)
			if _, err := allocationNetworkPeers([]any{p}, true); err == nil {
				t.Fatal("invalid existence selector accepted")
			}
		})
	}
	if _, err := allocationNetworkPeers([]any{existsPeer(), existsPeer()}, true); err == nil {
		t.Fatal("duplicate existence selector accepted")
	}
	parsed, err := allocationNetworkPeers([]any{existsPeer()}, true)
	if err != nil || len(parsed) != 1 || parsed[0].PodExistsLabel != "example.test/workload" || parsed[0].PodLabel != "" || parsed[0].PodValue != "" {
		t.Fatal(parsed, err)
	}
}

func TestAllocationNetworkPeerExistsCEL(t *testing.T) {
	peers, err := allocationNetworkPeers([]any{existsPeer()}, true)
	if err != nil {
		t.Fatal(err)
	}
	expression := allocationNetworkRulesCEL(allocationProfile{NetworkPeers: peers}, "ingress")
	env, err := cel.NewEnv(cel.Variable("variables.o", cel.MapType(cel.StringType, cel.DynType)), cel.Variable("namespaceObject", cel.MapType(cel.StringType, cel.DynType)))
	if err != nil {
		t.Fatal(err)
	}
	ast, issues := env.Compile(expression)
	if issues.Err() != nil {
		t.Fatal(issues.Err())
	}
	program, err := env.Program(ast, cel.CostLimit(10000))
	if err != nil {
		t.Fatal(err)
	}
	base := object{"namespaceSelector": object{"matchLabels": object{"kubernetes.io/metadata.name": "tenant-one"}}, "podSelector": object{"matchExpressions": []any{object{"key": "example.test/workload", "operator": "Exists"}}}}
	cases := map[string]struct {
		allowed bool
		change  func(object)
	}{
		"declared":     {true, func(object) {}},
		"empty values": {true, func(p object) { p["podSelector"].(object)["matchExpressions"].([]any)[0].(object)["values"] = []any{} }},
		"wrong key": {false, func(p object) {
			p["podSelector"].(object)["matchExpressions"].([]any)[0].(object)["key"] = "example.test/other"
		}},
		"absent key": {false, func(p object) {
			p["podSelector"].(object)["matchExpressions"].([]any)[0].(object)["operator"] = "DoesNotExist"
		}},
		"value restriction": {false, func(p object) {
			p["podSelector"].(object)["matchExpressions"].([]any)[0].(object)["values"] = []any{"one"}
		}},
		"extra expression": {false, func(p object) {
			s := p["podSelector"].(object)
			s["matchExpressions"] = append(s["matchExpressions"].([]any), object{"key": "other", "operator": "Exists"})
		}},
		"empty selector":   {false, func(p object) { p["podSelector"] = object{} }},
		"missing selector": {false, func(p object) { delete(p, "podSelector") }},
		"extra label":      {false, func(p object) { p["podSelector"].(object)["matchLabels"] = object{"app": "other"} }},
		"other namespace": {false, func(p object) {
			p["namespaceSelector"].(object)["matchLabels"].(object)["kubernetes.io/metadata.name"] = "other"
		}},
		"extra IP peer": {false, func(p object) { p["ipBlock"] = object{"cidr": "0.0.0.0/0"} }},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			raw, _ := json.Marshal(base)
			var peer object
			if err := json.Unmarshal(raw, &peer); err != nil {
				t.Fatal(err)
			}
			tc.change(peer)
			rule := object{"from": []any{peer}, "ports": []any{object{"protocol": "TCP", "port": 8080}}}
			value, _, err := program.ContextEval(context.Background(), map[string]any{"variables.o": object{"spec": object{"ingress": []any{rule}}}, "namespaceObject": object{"metadata": object{"name": "tenant-one"}}})
			allowed := err == nil && value.Value() == true
			if allowed != tc.allowed {
				t.Fatal("selector decision differs", value, err)
			}
		})
	}
}
