package kubernetesservice

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

func celString(value string) string { return strconv.Quote(value) }
func allocationMarker(config allocationConfiguration) string {
	return "{{allocationID .Namespace `" + config.Allocator + "`}}"
}
func allocationRoleName(config allocationConfiguration, role string) string {
	return "{{.Namespace}}." + config.Allocator + "." + role
}
func allocationPolicy(name string, rules []any, condition string, variables, validations []any) []any {
	return []any{
		object{"apiVersion": "admissionregistration.k8s.io/v1", "kind": "ValidatingAdmissionPolicy", "metadata": object{"name": name}, "spec": object{
			"failurePolicy": "Fail", "matchConstraints": object{"resourceRules": rules},
			"matchConditions": []any{object{"name": "selected-identity", "expression": condition}},
			"variables":       variables, "validations": validations,
		}},
		object{"apiVersion": "admissionregistration.k8s.io/v1", "kind": "ValidatingAdmissionPolicyBinding", "metadata": object{"name": name}, "spec": object{"policyName": name, "validationActions": []string{"Deny"}}},
	}
}
func allocationRule(group string, resources ...string) object {
	return object{"apiGroups": []string{group}, "apiVersions": []string{"v1"}, "operations": []string{"CREATE", "UPDATE", "DELETE"}, "resources": resources}
}
func allocationObjects(config allocationConfiguration) ([]any, error) {
	marker := allocationMarker(config)
	base := "{{.Namespace}}." + config.Allocator
	allocatorUser := "system:serviceaccount:{{.Namespace}}:" + config.Allocator
	isAllocator := "request.userInfo.username == " + celString(allocatorUser)
	markerExpr := celString(marker)
	marked := func(o string) string {
		return "(" + o + " != null && has(" + o + ".metadata.labels) && 'stego.dev/allocator' in " + o + ".metadata.labels && " + o + ".metadata.labels['stego.dev/allocator'] == " + markerExpr + ")"
	}
	owner := func(o string, p allocationProfile) string {
		return "(" + marked(o) + " && 'stego.dev/allocation-profile' in " + o + ".metadata.labels && " + o + ".metadata.labels['stego.dev/allocation-profile'] == " + celString(p.Name) +
			" && " + celString(p.OwnerLabel) + " in " + o + ".metadata.labels && " + o + ".metadata.labels[" + celString(p.OwnerLabel) + "].matches('^[A-Za-z0-9]([A-Za-z0-9_.-]{0,61}[A-Za-z0-9])?$') && 'app.kubernetes.io/managed-by' in " + o + ".metadata.labels && " + o + ".metadata.labels['app.kubernetes.io/managed-by'] == " + celString(p.Manager) + ")"
	}
	pattern := func(value string, p allocationProfile) string {
		suffix := "[a-z0-9]"
		if p.SuffixLength > 1 {
			suffix += fmt.Sprintf("[a-z0-9-]{%d}[a-z0-9]", p.SuffixLength-2)
		}
		return "(" + value + " != '{{.Namespace}}' && " + value + ".matches(" + celString("^"+p.Prefix+suffix+"$") + "))"
	}
	namespaceCases, quotaCases, bindingCases, clusterCases := []string{}, []string{}, []string{}, []string{}
	scope := map[string]string{}
	var items []any
	bindNames := []string{base + ".proof"}
	for _, r := range config.Roles {
		scope[r.Name] = r.Scope
		name := allocationRoleName(config, r.Name)
		bindNames = append(bindNames, name)
		items = append(items, object{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole", "metadata": object{"name": name}, "rules": r.Rules})
	}
	identityMaps := []string{}
	seenIdentityMaps := map[string]bool{}
	for _, p := range config.Profiles {
		if p.IdentityConfigMap != "" && !seenIdentityMaps[p.IdentityConfigMap] {
			seenIdentityMaps[p.IdentityConfigMap] = true
			identityMaps = append(identityMaps, p.IdentityConfigMap)
		}
	}
	sort.Strings(identityMaps)
	// This role gives no write access. Its binding is the authorization proof
	// for a namespace before the allocator can make a cluster binding for it.
	items = append(items, object{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole", "metadata": object{"name": base + ".proof"}, "rules": []any{object{"apiGroups": []string{"coordination.k8s.io"}, "resources": []string{"leases"}, "verbs": []string{"get"}, "resourceNames": []string{marker}}}})
	if len(identityMaps) > 0 {
		proof := items[len(items)-1].(object)
		proof["rules"] = append(proof["rules"].([]any), object{"apiGroups": []string{""}, "resources": []string{"configmaps"}, "verbs": []string{"get"}, "resourceNames": identityMaps})
	}
	binding := func(role, sa, ns, name string) string {
		return "(has(variables.o.roleRef) && variables.o.roleRef.apiGroup == 'rbac.authorization.k8s.io' && variables.o.roleRef.kind == 'ClusterRole' && variables.o.roleRef.name == " + celString(role) + " && has(variables.o.subjects) && size(variables.o.subjects) == 1 && variables.o.subjects[0].kind == 'ServiceAccount' && (!has(variables.o.subjects[0].apiGroup) || variables.o.subjects[0].apiGroup == '') && variables.o.subjects[0].name == " + celString(sa) + " && variables.o.subjects[0].namespace == " + ns + " && variables.o.metadata.name == " + name + ")"
	}
	for _, p := range config.Profiles {
		nsCase := "(" + owner("variables.o", p) + " && " + pattern("variables.o.metadata.name", p) + " && 'pod-security.kubernetes.io/enforce' in variables.o.metadata.labels && variables.o.metadata.labels['pod-security.kubernetes.io/enforce'] == 'restricted')"
		namespaceCases = append(namespaceCases, nsCase)
		within := "(" + owner("namespaceObject", p) + " && " + pattern("namespaceObject.metadata.name", p) + " && " + owner("variables.o", p) + " && variables.o.metadata.labels[" + celString(p.OwnerLabel) + "] == namespaceObject.metadata.labels[" + celString(p.OwnerLabel) + "] && variables.o.metadata.namespace == namespaceObject.metadata.name)"
		quotaKeys := make([]string, 0, len(p.Quota))
		for key := range p.Quota {
			quotaKeys = append(quotaKeys, key)
		}
		sort.Strings(quotaKeys)
		quotaChecks := []string{"has(variables.o.spec) && has(variables.o.spec.hard)", "size(variables.o.spec.hard) == 5"}
		for _, key := range quotaKeys {
			quotaChecks = append(quotaChecks, celString(key)+" in variables.o.spec.hard && quantity(variables.o.spec.hard["+celString(key)+"]).compareTo(quantity("+celString(p.Quota[key])+")) == 0")
		}
		bindingCases = append(bindingCases, "(request.operation == 'DELETE' && "+within+" && variables.o.metadata.name.matches("+celString("^stego-"+marker+"-([0-9]|1[0-5]|proof)$")+"))")
		clusterCases = append(clusterCases, "(request.operation == 'DELETE' && "+owner("variables.o", p)+" && has(variables.o.subjects) && size(variables.o.subjects) == 1 && "+pattern("variables.o.subjects[0].namespace", p)+" && variables.o.metadata.name.startsWith("+celString(base+".")+" + variables.o.subjects[0].namespace + '.') && variables.o.metadata.name.matches('.*[.]([0-9]|1[0-5])$'))")
		quotaCases = append(quotaCases, "("+within+" && variables.o.metadata.name == 'stego-allocation' && "+strings.Join(quotaChecks, " && ")+")")
		proof := binding(base+".proof", config.Allocator, "'{{.Namespace}}'", celString("stego-"+marker+"-proof"))
		bindingCases = append(bindingCases, "("+within+" && "+proof+")")
		for i, b := range p.Bindings {
			roleName := b.ExternalRole
			if b.Role != "" {
				roleName = allocationRoleName(config, b.Role)
			} else {
				bindNames = append(bindNames, roleName)
			}
			subjectNamespace := "'{{.Namespace}}'"
			if b.Namespace == "allocated" {
				subjectNamespace = "namespaceObject.metadata.name"
			}
			if scope[b.Role] == "cluster" {
				subjectNamespace = "variables.o.subjects[0].namespace"
				own := owner("variables.o", p)
				name := celString(base+".") + " + " + subjectNamespace + " + " + celString(fmt.Sprintf(".%d", i))
				proof := "authorizer.group('coordination.k8s.io').resource('leases').namespace(" + subjectNamespace + ").name(" + markerExpr + ").check('get').allowed()"
				clusterCases = append(clusterCases, "("+own+" && "+binding(roleName, b.ServiceAccount, subjectNamespace, name)+" && "+pattern(subjectNamespace, p)+" && (request.operation == 'DELETE' || "+proof+"))")
			} else {
				bindingCases = append(bindingCases, "("+within+" && "+binding(roleName, b.ServiceAccount, subjectNamespace, celString(fmt.Sprintf("stego-%s-%d", marker, i)))+")")
			}
		}
	}
	join := func(values []string) string {
		if len(values) == 0 {
			return "false"
		}
		return "(" + strings.Join(values, " || ") + ")"
	}
	variables := []any{object{"name": "o", "expression": "object != null ? object : oldObject"}}
	validate := func(expression, message string) object { return object{"expression": expression, "message": message} }
	sameOwner := []string{"object.metadata.labels['stego.dev/allocation-profile'] == oldObject.metadata.labels['stego.dev/allocation-profile']", "object.metadata.labels['app.kubernetes.io/managed-by'] == oldObject.metadata.labels['app.kubernetes.io/managed-by']"}
	for _, p := range config.Profiles {
		sameOwner = append(sameOwner, "(oldObject.metadata.labels['stego.dev/allocation-profile'] != "+celString(p.Name)+" || object.metadata.labels["+celString(p.OwnerLabel)+"] == oldObject.metadata.labels["+celString(p.OwnerLabel)+"])")
	}
	checks := []any{
		validate("request.operation != 'UPDATE' || ("+strings.Join(sameOwner, " && ")+")", "Allocator cannot change resource ownership"),
		validate("request.resource.resource != 'namespaces' || "+join(namespaceCases), "Namespace must match its allocation profile"),
		validate("request.resource.resource != 'resourcequotas' || "+join(quotaCases), "Quota must match its allocation profile"),
		validate("request.resource.resource != 'rolebindings' || "+join(bindingCases), "Binding must match its allocated namespace"),
		validate("request.resource.resource != 'clusterrolebindings' || "+join(clusterCases), "Cluster binding requires an allocated namespace and a declared role"),
		validate("oldObject == null || "+marked("oldObject"), "Allocator cannot change a foreign resource"),
	}
	guarded := allocationPolicy(base+".allocation", []any{allocationRule("", "namespaces", "resourcequotas"), allocationRule("rbac.authorization.k8s.io", "rolebindings", "clusterrolebindings")}, isAllocator, variables, checks)
	// Namespace identity cannot be added to an existing foreign namespace, removed,
	// or changed. The policy applies to all actors, including a worker with a bug.
	unchanged := []string{"object.metadata.labels['stego.dev/allocator'] == oldObject.metadata.labels['stego.dev/allocator']", "object.metadata.labels['stego.dev/allocation-profile'] == oldObject.metadata.labels['stego.dev/allocation-profile']", "object.metadata.labels['app.kubernetes.io/managed-by'] == oldObject.metadata.labels['app.kubernetes.io/managed-by']", "object.metadata.labels['pod-security.kubernetes.io/enforce'] == 'restricted'"}
	for _, p := range config.Profiles {
		unchanged = append(unchanged, "(oldObject.metadata.labels['stego.dev/allocation-profile'] != "+celString(p.Name)+" || object.metadata.labels["+celString(p.OwnerLabel)+"] == oldObject.metadata.labels["+celString(p.OwnerLabel)+"])")
	}
	for _, p := range config.Profiles {
		for _, group := range []struct {
			name   string
			fields []allocationIdentityField
		}{{"labels", p.IdentityLabels}, {"annotations", p.IdentityAnnotations}} {
			for _, field := range group.fields {
				old := "oldObject.metadata." + group.name
				next := "object.metadata." + group.name
				key := celString(field.Key)
				oldHas := "(has(" + old + ") && " + key + " in " + old + ")"
				newHas := "(has(" + next + ") && " + key + " in " + next + ")"
				unchanged = append(unchanged, "(oldObject.metadata.labels['stego.dev/allocation-profile'] != "+celString(p.Name)+" || ("+oldHas+" ? ("+newHas+" && "+next+"["+key+"] == "+old+"["+key+"]) : (!"+newHas+" || "+isAllocator+")))")
			}
		}
	}
	immutable := "request.operation == 'DELETE' || (request.operation == 'CREATE' ? (" + isAllocator + " && " + join(namespaceCases) + ") : (" + marked("oldObject") + " && " + marked("object") + " && " + strings.Join(unchanged, " && ") + "))"
	guarded = append(guarded, allocationPolicy(base+".ownership", []any{allocationRule("", "namespaces")}, marked("object")+" || "+marked("oldObject"), variables, []any{validate(immutable, "Allocation identity and restricted Pod security are immutable")})...)
	// A namespace worker must not remove the quota or change reserved bindings,
	// even if an external role has broad permissions in that namespace.
	reserved := "!has(request.name) || (request.resource.resource == 'resourcequotas' ? request.name == 'stego-allocation' : request.name.startsWith(" + celString("stego-"+marker+"-") + "))"
	resourceRule := "!(" + marked("namespaceObject") + ") || " + isAllocator + " || (request.operation == 'DELETE' && has(namespaceObject.metadata.deletionTimestamp))"
	guarded = append(guarded, allocationPolicy(base+".resources", []any{allocationRule("", "resourcequotas"), allocationRule("rbac.authorization.k8s.io", "rolebindings")}, reserved, variables, []any{validate(resourceRule, "Only the allocator can change allocation limits and bindings")})...)
	// Install the policies before the allocator receives permissions.
	items = append(guarded, items...)
	sort.Strings(bindNames)
	unique := bindNames[:0]
	for _, v := range bindNames {
		if len(unique) == 0 || unique[len(unique)-1] != v {
			unique = append(unique, v)
		}
	}
	rules := []any{
		object{"apiGroups": []string{""}, "resources": []string{"namespaces"}, "verbs": []string{"get", "list", "create", "delete"}},
		object{"apiGroups": []string{""}, "resources": []string{"resourcequotas"}, "verbs": []string{"get", "create", "patch"}},
		object{"apiGroups": []string{"rbac.authorization.k8s.io"}, "resources": []string{"rolebindings", "clusterrolebindings"}, "verbs": []string{"get", "list", "create", "patch", "delete"}},
		object{"apiGroups": []string{"rbac.authorization.k8s.io"}, "resources": []string{"clusterroles"}, "resourceNames": unique, "verbs": []string{"bind"}},
	}
	if len(identityMaps) > 0 {
		rules[0].(object)["verbs"] = []string{"get", "list", "create", "patch", "delete"}
	}
	items = append(items, object{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole", "metadata": object{"name": base}, "rules": rules}, object{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRoleBinding", "metadata": object{"name": base}, "roleRef": object{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": base}, "subjects": []any{object{"kind": "ServiceAccount", "name": config.Allocator, "namespace": "{{.Namespace}}"}}})
	// Keep the policy source below the API server object limit.
	data, err := json.Marshal(items)
	if err != nil {
		return nil, err
	}
	if len(data) > 200000 {
		return nil, fmt.Errorf("allocation policy exceeds its size limit")
	}
	return items, nil
}
