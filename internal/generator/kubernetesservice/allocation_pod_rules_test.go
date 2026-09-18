package kubernetesservice

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"cel.dev/cel-go/cel"
)

func TestAllocationPodRuleValidation(t *testing.T) {
	for name, raw := range map[string]any{
		"null": nil, "wrong type": "true", "empty": []any{}, "too many": make([]any, 17),
		"missing message": []any{object{"expression": "true"}},
		"unknown field":   []any{object{"expression": "true", "message": "Denied", "failurePolicy": "Ignore"}},
	} {
		t.Run(name, func(t *testing.T) {
			c := isolatedAllocationContext()
			c.ComponentConfig["allocation_profiles"].([]any)[0].(object)["pod_validations"] = raw
			files, wiring, err := new(Generator).Generate(c)
			if err == nil || files != nil || wiring != nil {
				t.Fatal("invalid rule emitted output")
			}
		})
	}
	for name, expression := range map[string]string{
		"empty expression": "", "syntax": "object.spec.(", "unknown variable": "objects.spec == null",
		"unknown function": "object.spec.ignore()", "number": "1", "dynamic result": "object.spec",
		"string": "'true'", "template": "'{{.Namespace}}' == 'x'", "oversize": strings.Repeat("x", 4097),
		"deep expression":         strings.Repeat("(", 100) + "true" + strings.Repeat(")", 100),
		"unknown policy variable": "variables.other.size() > 0", "invalid utf8": "'\xff' == 'x'",
	} {
		t.Run(name, func(t *testing.T) {
			c := isolatedAllocationContext()
			c.ComponentConfig["allocation_profiles"].([]any)[0].(object)["pod_validations"] = []any{object{"expression": expression, "message": "Application rule denied this Pod"}}
			files, wiring, err := new(Generator).Generate(c)
			if err == nil || files != nil || wiring != nil {
				t.Fatal("invalid CEL emitted output")
			}
		})
	}
	for _, message := range []any{"", "line\nbreak", "{{.Namespace}}", strings.Repeat("x", 257), true} {
		c := isolatedAllocationContext()
		c.ComponentConfig["allocation_profiles"].([]any)[0].(object)["pod_validations"] = []any{object{"expression": "true", "message": message}}
		if files, wiring, err := new(Generator).Generate(c); err == nil || files != nil || wiring != nil {
			t.Fatal("invalid message emitted output")
		}
	}
	c := allocationContext()
	c.ComponentConfig["allocation_profiles"].([]any)[0].(object)["pod_validations"] = []any{object{"expression": "true", "message": "Denied"}}
	if _, _, err := new(Generator).Generate(c); err == nil {
		t.Fatal("rule without a Pod policy was accepted")
	}
}

func TestAllocationPodRulesKeepCommonGuards(t *testing.T) {
	c := isolatedAllocationContext()
	before, err := allocationConfig(c)
	if err != nil {
		t.Fatal(err)
	}
	base := allocationPodObjects(before, before.Profiles[0], "true")
	rule := object{"expression": "true", "message": "Application rule"}
	p := c.ComponentConfig["allocation_profiles"].([]any)[0].(object)
	p["pod_validations"] = []any{rule}
	after, err := allocationConfig(c)
	if err != nil {
		t.Fatal(err)
	}
	objects := allocationPodObjects(after, after.Profiles[0], "true")
	spec := objects[0].(object)["spec"].(object)
	checks := spec["validations"].([]any)
	if !reflect.DeepEqual(checks[len(checks)-1], rule) {
		t.Fatal("application rule changed")
	}
	spec["validations"] = checks[:len(checks)-1]
	if !reflect.DeepEqual(objects, base) {
		t.Fatal("application rule changed common policy or binding")
	}
	encoded, err := json.Marshal(after)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "Application rule") {
		t.Fatal("worker interprets application admission code")
	}
	p["pod_validations"] = []any{rule, rule}
	if _, _, err := new(Generator).Generate(c); err == nil {
		t.Fatal("duplicate rule was accepted")
	}
}

func TestAllocationPodRuleCredentialMounts(t *testing.T) {
	// Common storage permissions allow Secrets. The application adds its own
	// credential separation without giving its worker policy-write permission.
	expression := "variables.containers.all(c, c.name != 'workload' || !has(c.volumeMounts) || c.volumeMounts.all(m, m.name != 'client-identity'))"
	c := isolatedAllocationContext()
	c.ComponentConfig["allocation_profiles"].([]any)[0].(object)["pod_validations"] = []any{object{"expression": expression, "message": "Workload cannot mount client identity"}}
	config, err := allocationConfig(c)
	if err != nil {
		t.Fatal(err)
	}
	if config.Profiles[0].PodValidations[0].Expression != expression {
		t.Fatal("application rule was lost")
	}
	env, err := cel.NewEnv(cel.Variable("variables.containers", cel.ListType(cel.DynType)))
	if err != nil {
		t.Fatal(err)
	}
	ast, issues := env.Compile(expression)
	if issues.Err() != nil {
		t.Fatal(issues.Err())
	}
	program, err := env.Program(ast, cel.CostLimit(1000))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name       string
		containers []any
		allow      bool
	}{
		{"workload without credential", []any{object{"name": "workload"}}, true},
		{"credential helper", []any{object{"name": "helper", "volumeMounts": []any{object{"name": "client-identity"}}}}, true},
		{"credential exposed to workload", []any{object{"name": "workload", "volumeMounts": []any{object{"name": "client-identity"}}}}, false},
		{"workspace volume", []any{object{"name": "workload", "volumeMounts": []any{object{"name": "workspace"}}}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, _, err := program.Eval(map[string]any{"variables.containers": tc.containers})
			if err != nil || value.Value() != tc.allow {
				t.Fatal("incorrect credential policy result", value, err)
			}
		})
	}
}

func TestApplicationAllocationManifests(t *testing.T) {
	t.Setenv("STEGO_ALLOCATION_SERVICE_ACCOUNTS", "1")
	c := isolatedAllocationContext()
	p := c.ComponentConfig["allocation_profiles"].([]any)[0].(object)
	p["bindings"] = append(p["bindings"].([]any), object{"external_role": "system:openshift:scc:privileged", "service_account": "gateway", "namespace": "allocated"})
	p["pod_validations"] = []any{object{"expression": "variables.containers.all(c, !(c.name in ['check','workspace-copy']) || !has(c.volumeMounts) || c.volumeMounts.all(m, m.name != 'client-identity'))", "message": "Workload and workspace cannot mount client identity"}}
	p["network_peers"] = []any{object{"direction": "egress", "namespace": "profile", "peer_profile": "peer", "pod_label": "app", "pod_value": "service", "port": 8080, "protocol": "TCP"}}
	peer := allocationContext().ComponentConfig["allocation_profiles"].([]any)[0].(object)
	peer["name"] = "peer"
	peer["namespace_prefix"] = "stego-rules-target-ci-"
	peer["network_isolation"] = true
	c.ComponentConfig["allocation_profiles"] = []any{p, peer}
	testAllocationManifests(t, c)
}
