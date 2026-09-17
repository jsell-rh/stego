package kubernetesservice

import (
	"fmt"
	"strings"
)

func allocationPodConfig(values object, p *allocationProfile) error {
	if raw, exists := values["pod_runtime_class"]; exists {
		name, ok := raw.(string)
		if !ok || name == "" || len(name) > 253 {
			return fmt.Errorf("pod_runtime_class requires a DNS subdomain")
		}
		for _, part := range strings.Split(name, ".") {
			if !label.MatchString(part) {
				return fmt.Errorf("pod_runtime_class requires a DNS subdomain")
			}
		}
		p.PodRuntimeClass = name
	}
	if raw, exists := values["pod_service_account"]; exists {
		alias, ok := raw.(string)
		if !ok || alias == "" || !allocationHasServiceAccount(*p, alias) {
			return fmt.Errorf("pod_service_account requires a declared local ServiceAccount alias")
		}
		p.PodServiceAccount = alias
	}
	return nil
}

// These rules add restrictions to the immutable restricted Pod security level.
// The operator installs them before it grants allocator or workload access.
func allocationPodObjects(config allocationConfiguration, p allocationProfile, identity string) []any {
	if p.PodRuntimeClass == "" && p.PodServiceAccount == "" {
		return nil
	}
	checks := []any{object{
		"expression": "namespaceObject != null && " + identity + " && namespaceObject.metadata.labels['pod-security.kubernetes.io/enforce'] == 'restricted'",
		"message":    "Pod requires a current allocation with restricted Pod security",
	}}
	if p.PodRuntimeClass != "" {
		checks = append(checks, object{
			"expression": "has(object.spec.runtimeClassName) && object.spec.runtimeClassName == " + celString(p.PodRuntimeClass),
			"message":    "Pod must use the declared RuntimeClass",
		})
	}
	if p.PodServiceAccount != "" {
		checks = append(checks, object{
			"expression": "namespaceObject != null && " + allocationServiceAccountAnnotationCEL(config, "namespaceObject", p.PodServiceAccount) + " && object.spec.serviceAccountName == " + allocationServiceAccountCEL(p.PodServiceAccount) + " && has(object.spec.automountServiceAccountToken) && !object.spec.automountServiceAccountToken",
			"message":    "Pod must use its allocated ServiceAccount and disable automatic token mounting",
		})
	}
	name := "{{.Namespace}}." + config.Allocator + ".pods." + p.Name
	rules := []any{object{"apiGroups": []string{""}, "apiVersions": []string{"v1"}, "operations": []string{"CREATE", "UPDATE"}, "resources": []string{"pods", "pods/ephemeralcontainers"}, "scope": "Namespaced"}}
	items := allocationPolicy(name, rules, "request.namespace != '{{.Namespace}}'", nil, checks)
	// Select immutable allocator and profile labels. Two installations can use
	// the same name pattern without applying their policy to each other's Pods.
	items[0].(object)["spec"].(object)["matchConstraints"].(object)["namespaceSelector"] = object{"matchLabels": object{
		"stego.dev/allocator": allocationMarker(config), "stego.dev/allocation-profile": p.Name,
	}}
	return items
}
