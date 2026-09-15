package kubernetesservice

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestAllocationNetworkIsolationManifest(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "isolated"}[enabled], func(t *testing.T) {
			c := allocationContext()
			p := c.ComponentConfig["allocation_profiles"].([]any)[0].(object)
			p["network_isolation"] = enabled
			other := object{}
			for k, v := range p {
				other[k] = v
			}
			other["name"] = "other"
			other["namespace_prefix"] = "other-"
			other["network_isolation"] = false
			c.ComponentConfig["allocation_profiles"] = append(c.ComponentConfig["allocation_profiles"].([]any), other)
			config, err := allocationConfig(c)
			if err != nil {
				t.Fatal(err)
			}
			if config.Profiles[0].NetworkIsolation != enabled || config.Profiles[1].NetworkIsolation {
				t.Fatal("profile isolation changed")
			}
			items, err := allocationObjects(config)
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(items)
			if err != nil {
				t.Fatal(err)
			}
			var objects []object
			if err = json.Unmarshal(data, &objects); err != nil {
				t.Fatal(err)
			}
			rules, guards, reserved := 0, 0, 0
			for _, item := range objects {
				if item["kind"] == "ClusterRole" {
					for _, raw := range item["rules"].([]any) {
						rule := raw.(object)
						if !reflect.DeepEqual(rule["apiGroups"], []any{"networking.k8s.io"}) {
							continue
						}
						rules++
						if !reflect.DeepEqual(rule["resources"], []any{"networkpolicies"}) {
							t.Fatal("unexpected network resource permission")
						}
						if reflect.DeepEqual(rule["verbs"], []any{"get"}) {
							if !reflect.DeepEqual(rule["resourceNames"], []any{"stego-allocation"}) {
								t.Fatal("network read is not restricted by name")
							}
						} else if !reflect.DeepEqual(rule["verbs"], []any{"create"}) {
							t.Fatal("allocator can change or delete a policy")
						}
					}
				}
				if item["kind"] != "ValidatingAdmissionPolicy" {
					continue
				}
				spec := item["spec"].(object)
				for _, raw := range spec["validations"].([]any) {
					expression := raw.(object)["expression"].(string)
					if strings.Contains(expression, "NetworkPolicy") {
						t.Fatal("resource kind used in place of the API resource")
					}
					if strings.HasPrefix(expression, "request.resource.resource != 'networkpolicies'") {
						guards++
						for _, part := range []string{"stego-allocation", "request.operation != 'DELETE'", "'Ingress' in", "'Egress' in", "size(variables.o.spec.policyTypes) == 2", "size(variables.o.spec.ingress) == 0", "size(variables.o.spec.egress) == 0", "size(variables.o.spec.podSelector.matchLabels) == 0", "size(variables.o.spec.podSelector.matchExpressions) == 0", "namespaceObject.metadata.name", "\"tenant\""} {
							if !strings.Contains(expression, part) {
								t.Fatal("missing network constraint", part)
							}
						}
						if strings.Contains(expression, "\"other\"") {
							t.Fatal("network permission includes a profile that did not select isolation")
						}
					}
				}
				for _, raw := range spec["matchConditions"].([]any) {
					if strings.Contains(raw.(object)["expression"].(string), "request.resource.resource == 'networkpolicies'") {
						reserved++
					}
				}
			}
			if enabled && (rules != 2 || guards != 1 || reserved != 1) {
				t.Fatal("network protection missing", rules, guards, reserved)
			}
			if !enabled && (rules != 0 || guards != 0 || reserved != 0) {
				t.Fatal("legacy profile gained network permissions")
			}
			files, err := allocationFiles(c)
			if err != nil {
				t.Fatal(err)
			}
			serialized := false
			for _, f := range files {
				serialized = serialized || bytes.Contains(f.Bytes(), []byte(`\"NetworkIsolation\":true`))
			}
			if serialized != enabled {
				t.Fatal("generated runtime lost the network setting")
			}
			again, err := allocationObjects(config)
			if err != nil {
				t.Fatal(err)
			}
			repeated, err := json.Marshal(again)
			if err != nil || !bytes.Equal(data, repeated) {
				t.Fatal("network generation is unstable")
			}
		})
	}
}
