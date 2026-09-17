package kubernetesservice

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAllocationControlAccountManifests(t *testing.T) {
	// Fixed control identities require protection even without managed workload
	// accounts. Related profiles must not add their workload aliases here.
	for _, related := range []bool{false, true} {
		ctx := allocationContext()
		if related {
			ctx = peerAllocationContext()
		}
		config, err := allocationConfig(ctx)
		if err != nil {
			t.Fatal(err)
		}
		items, err := allocationObjects(config)
		if err != nil {
			t.Fatal(err)
		}
		base := "{{.Namespace}}." + config.Allocator
		capability := allocationControlAccountCapability + "{{.Namespace}}/" + config.Allocator
		guard, installer := false, false
		for _, raw := range items {
			item := raw.(object)
			name := item["metadata"].(object)["name"]
			encoded, err := json.Marshal(item)
			if err != nil {
				t.Fatal(err)
			}
			if item["kind"] == "ValidatingAdmissionPolicy" && name == base+".control-accounts" {
				guard = true
				spec := item["spec"].(object)
				condition := spec["matchConditions"].([]any)[0].(object)["expression"].(string)
				if condition != "request.namespace == '{{.Namespace}}' && request.operation == 'CREATE'" || spec["failurePolicy"] != "Fail" {
					t.Fatal("control account guard has a wrong scope or failure mode")
				}
				expression := spec["validations"].([]any)[0].(object)["expression"].(string)
				for _, required := range []string{`["widget-data","widget-queue"]`, "object.metadata.generateName", "authorizer.path(" + celString(capability) + ")"} {
					if !strings.Contains(expression, required) {
						t.Fatal("control account reservation is incomplete", required)
					}
				}
				if strings.Contains(expression, "namespaceObject") || strings.Contains(expression, "request.userInfo") || strings.Contains(expression, `"gateway"`) || strings.Contains(expression, `"runner"`) {
					t.Fatal("control account guard has an unrelated identity or implicit exemption")
				}
			} else if item["kind"] == "ClusterRole" && name == base+".control-account-installer" {
				installer = true
				rules := item["rules"].([]any)
				if len(rules) != 1 {
					t.Fatal("installer role has extra permissions")
				}
				rule := rules[0].(object)
				if len(rule) != 2 || strings.Join(rule["nonResourceURLs"].([]string), ",") != capability || strings.Join(rule["verbs"].([]string), ",") != "get" {
					t.Fatal("installer capability is not limited to this installation")
				}
			} else if strings.Contains(string(encoded), allocationControlAccountCapability) {
				t.Fatal("installer capability leaked to another generated object", name)
			}
			if item["kind"] == "ClusterRoleBinding" && item["roleRef"].(object)["name"] == base+".control-account-installer" {
				t.Fatal("installer role was bound automatically")
			}
		}
		if !guard || !installer {
			t.Fatal("control account protection is missing")
		}
	}
}
