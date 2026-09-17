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
	networkCases := []string{}
	serviceAccountCases := []string{}
	serviceAccountProfiles := []string{}
	serviceAccountNamespacePatterns := []string{}
	isolatedNames := []string{}
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
		return "(has(variables.o.roleRef) && variables.o.roleRef.apiGroup == 'rbac.authorization.k8s.io' && variables.o.roleRef.kind == 'ClusterRole' && variables.o.roleRef.name == " + celString(role) + " && has(variables.o.subjects) && size(variables.o.subjects) == 1 && variables.o.subjects[0].kind == 'ServiceAccount' && (!has(variables.o.subjects[0].apiGroup) || variables.o.subjects[0].apiGroup == '') && variables.o.subjects[0].name == " + sa + " && variables.o.subjects[0].namespace == " + ns + " && variables.o.metadata.name == " + name + ")"
	}
	for _, p := range config.Profiles {
		nsCase := "(" + owner("variables.o", p) + " && " + pattern("variables.o.metadata.name", p) + " && 'pod-security.kubernetes.io/enforce' in variables.o.metadata.labels && variables.o.metadata.labels['pod-security.kubernetes.io/enforce'] == 'restricted')"
		for _, alias := range allocationAccountIdentities(p) {
			nsCase += " && " + allocationServiceAccountAnnotationCEL(config, "variables.o", alias)
		}
		namespaceCases = append(namespaceCases, "("+nsCase+")")
		within := "(" + owner("namespaceObject", p) + " && " + pattern("namespaceObject.metadata.name", p) + " && " + owner("variables.o", p) + " && variables.o.metadata.labels[" + celString(p.OwnerLabel) + "] == namespaceObject.metadata.labels[" + celString(p.OwnerLabel) + "] && variables.o.metadata.namespace == namespaceObject.metadata.name)"
		if len(p.ServiceAccounts) > 0 {
			serviceAccountProfiles = append(serviceAccountProfiles, celString(p.Name))
			generated := "(has(object.metadata.generateName) && object.metadata.generateName != '' && (object.metadata.generateName.startsWith(" + celString(p.Prefix) + ") || " + celString(p.Prefix) + ".startsWith(object.metadata.generateName)))"
			serviceAccountNamespacePatterns = append(serviceAccountNamespacePatterns, "((has(object.metadata.name) && "+pattern("object.metadata.name", p)+") || "+generated+")")
			choices := []string{}
			for _, alias := range p.ServiceAccounts {
				choices = append(choices, "variables.o.metadata.name == "+allocationServiceAccountCEL(alias))
			}
			serviceAccountCases = append(serviceAccountCases, "("+within+" && ("+strings.Join(choices, " || ")+") && has(variables.o.automountServiceAccountToken) && variables.o.automountServiceAccountToken == false)")
		}
		if p.NetworkIsolation {
			isolatedNames = append(isolatedNames, celString(p.Name))
			deny := "has(variables.o.spec) && has(variables.o.spec.podSelector) && (!has(variables.o.spec.podSelector.matchLabels) || size(variables.o.spec.podSelector.matchLabels) == 0) && (!has(variables.o.spec.podSelector.matchExpressions) || size(variables.o.spec.podSelector.matchExpressions) == 0) && has(variables.o.spec.policyTypes) && size(variables.o.spec.policyTypes) == 2 && 'Ingress' in variables.o.spec.policyTypes && 'Egress' in variables.o.spec.policyTypes && " + allocationNetworkRulesCEL(p, "ingress") + " && " + allocationNetworkRulesCEL(p, "egress")
			networkCases = append(networkCases, "("+within+" && request.operation != 'DELETE' && variables.o.metadata.name == 'stego-allocation' && "+deny+")")
		}
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
		proof := binding(base+".proof", celString(config.Allocator), "'{{.Namespace}}'", celString("stego-"+marker+"-proof"))
		bindingCases = append(bindingCases, "("+within+" && "+proof+")")
		for i, b := range p.Bindings {
			roleName := b.ExternalRole
			if b.Role != "" {
				roleName = allocationRoleName(config, b.Role)
			} else {
				bindNames = append(bindNames, roleName)
			}
			subjectNamespace := "'{{.Namespace}}'"
			if b.Namespace == "external" {
				subjectNamespace = celString(b.ExternalNamespace)
			}
			if b.Namespace == "allocated" {
				subjectNamespace = "namespaceObject.metadata.name"
			}
			if b.Namespace == "profile" {
				subjectNamespace = celString(b.SubjectPrefix) + " + namespaceObject.metadata.name.substring(" + fmt.Sprint(len(p.Prefix)) + ")"
			}
			serviceAccount := celString(b.ServiceAccount)
			accountCheck := "true"
			managedAccount := (b.Namespace == "allocated" && allocationHasServiceAccount(p, b.ServiceAccount)) || b.Namespace == "profile"
			if managedAccount {
				serviceAccount = allocationServiceAccountCEL(b.ServiceAccount)
			}
			if scope[b.Role] == "cluster" {
				if managedAccount {
					accountCheck = allocationServiceAccountAnnotationCEL(config, "variables.o", b.ServiceAccount)
					serviceAccount = "variables.o.metadata.annotations[" + celString(allocationServiceAccountPrefix+b.ServiceAccount) + "]"
				}
				subjectNamespace = "variables.o.subjects[0].namespace"
				own := owner("variables.o", p)
				name := celString(base+".") + " + " + subjectNamespace + " + " + celString(fmt.Sprintf(".%d", i))
				proof := "authorizer.group('coordination.k8s.io').resource('leases').namespace(" + subjectNamespace + ").name(" + markerExpr + ").check('get').allowed()"
				clusterCases = append(clusterCases, "("+own+" && "+binding(roleName, serviceAccount, subjectNamespace, name)+" && "+accountCheck+" && "+pattern(subjectNamespace, p)+" && (request.operation == 'DELETE' || "+proof+"))")
			} else {
				bindingCases = append(bindingCases, "("+within+" && "+binding(roleName, serviceAccount, subjectNamespace, celString(fmt.Sprintf("stego-%s-%d", marker, i)))+")")
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
	allocationRules := []any{allocationRule("", "namespaces", "resourcequotas"), allocationRule("rbac.authorization.k8s.io", "rolebindings", "clusterrolebindings")}
	if len(networkCases) > 0 {
		allocationRules = append(allocationRules, allocationRule("networking.k8s.io", "networkpolicies"))
		checks = append(checks, validate("request.resource.resource != 'networkpolicies' || "+join(networkCases), "Network policy must match its allocation profile"))
	}
	if len(serviceAccountCases) > 0 {
		allocationRules = append(allocationRules, allocationRule("", "serviceaccounts"))
		checks = append(checks, validate("request.resource.resource != 'serviceaccounts' || "+join(serviceAccountCases), "ServiceAccount must match its allocation owner"))
	}
	guarded := allocationPolicy(base+".allocation", allocationRules, isAllocator, variables, checks)
	if refs := allocationEndpointReferences(config); len(refs) > 0 {
		encoded, err := json.Marshal(refs)
		if err != nil {
			return nil, err
		}
		policy := guarded[0].(object)
		policy["metadata"].(object)["annotations"] = object{allocationEndpointAnnotation: string(encoded)}
		policy["spec"].(object)["variables"] = append(append([]any{}, variables...), object{"name": "networkEndpoints", "expression": allocationEndpointPlaceholder})
	}

	// Namespace identity cannot be added to an existing foreign namespace, removed,
	// or changed. The policy applies to all actors, including a worker with a bug.
	unchanged := []string{"object.metadata.labels['stego.dev/allocator'] == oldObject.metadata.labels['stego.dev/allocator']", "object.metadata.labels['stego.dev/allocation-profile'] == oldObject.metadata.labels['stego.dev/allocation-profile']", "object.metadata.labels['app.kubernetes.io/managed-by'] == oldObject.metadata.labels['app.kubernetes.io/managed-by']", "object.metadata.labels['pod-security.kubernetes.io/enforce'] == 'restricted'"}
	for _, p := range config.Profiles {
		unchanged = append(unchanged, "(oldObject.metadata.labels['stego.dev/allocation-profile'] != "+celString(p.Name)+" || object.metadata.labels["+celString(p.OwnerLabel)+"] == oldObject.metadata.labels["+celString(p.OwnerLabel)+"])")
	}
	for _, p := range config.Profiles {
		for _, alias := range allocationAccountIdentities(p) {
			key := celString(allocationServiceAccountPrefix + alias)
			oldHas := "(has(oldObject.metadata.annotations) && " + key + " in oldObject.metadata.annotations)"
			newHas := "(has(object.metadata.annotations) && " + key + " in object.metadata.annotations)"
			unchanged = append(unchanged, "(oldObject.metadata.labels['stego.dev/allocation-profile'] != "+celString(p.Name)+" || ("+oldHas+" ? ("+newHas+" && object.metadata.annotations["+key+"] == oldObject.metadata.annotations["+key+"]) : ("+isAllocator+" && "+allocationServiceAccountAnnotationCEL(config, "object", alias)+")))")
		}
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
	resourceRules := []any{allocationRule("", "resourcequotas"), allocationRule("rbac.authorization.k8s.io", "rolebindings")}
	message := "Only the allocator can change allocation limits and bindings"
	if len(networkCases) > 0 {
		resourceRules = append(resourceRules, allocationRule("networking.k8s.io", "networkpolicies"))
		reserved = "(request.resource.resource == 'networkpolicies' || (" + reserved + "))"
		isolated := marked("namespaceObject") + " && 'stego.dev/allocation-profile' in namespaceObject.metadata.labels && namespaceObject.metadata.labels['stego.dev/allocation-profile'] in [" + strings.Join(isolatedNames, ",") + "]"
		resourceRule = "(request.resource.resource == 'networkpolicies' && has(request.name) && request.name != 'stego-allocation' && !(" + isolated + ")) || (" + resourceRule + ")"
		message = "Only the allocator can change allocation limits, bindings, and isolated network policies"
	}
	guarded = append(guarded, allocationPolicy(base+".resources", resourceRules, reserved, variables, []any{validate(resourceRule, message)})...)
	if len(serviceAccountCases) > 0 {
		// Kubernetes 1.35 passes no namespace object to match conditions.
		// Select by the request namespace, then check ownership in validation.
		requestPatterns := []string{}
		for _, p := range config.Profiles {
			if len(p.ServiceAccounts) > 0 {
				requestPatterns = append(requestPatterns, pattern("request.namespace", p))
			}
		}
		selected := join(requestPatterns)
		ownedProfile := "(" + marked("namespaceObject") + " && 'stego.dev/allocation-profile' in namespaceObject.metadata.labels && namespaceObject.metadata.labels['stego.dev/allocation-profile'] in [" + strings.Join(serviceAccountProfiles, ",") + "])"
		// Kubernetes can maintain the default account and image pull references.
		// Only the allocator can create a declared owner account. Updates must
		// retain the owner, generated name, and explicit token opt-in default.
		allowed := "variables.o.metadata.name == 'default' || (" + join(serviceAccountCases) + " && (" + isAllocator + " || request.operation == 'UPDATE' || (request.operation == 'DELETE' && has(namespaceObject.metadata.deletionTimestamp))))"
		allowed = "namespaceObject != null && (!(" + ownedProfile + ") || (" + allowed + "))"
		guarded = append(guarded, allocationPolicy(base+".service-accounts", []any{allocationRule("", "serviceaccounts")}, selected, variables, []any{validate(allowed, "ServiceAccount identity must match its allocation owner")})...)
		// An unmarked replacement namespace must not recreate a prior owner's
		// account. A common RBAC capability permits other trusted allocator
		// installations to use the same patterns under their own owner rules.
		reserved := "request.operation == 'CREATE' && object != null && " + join(serviceAccountNamespacePatterns)
		capable := "authorizer.path(" + celString(allocationNamespaceCapability) + ").check('get').allowed()"
		guarded = append(guarded, allocationPolicy(base+".namespace-reservations", []any{allocationRule("", "namespaces")}, reserved, nil, []any{validate(capable, "Allocated namespace patterns require a trusted allocator")})...)
		// A different trusted installation must not create this installation's
		// account names, even if its profile uses literal account names.
		generatedAccount := "((has(variables.o.metadata.name) && variables.o.metadata.name.matches('^sa-[0-9a-f]{32}-[a-z2-7]{26}$')) || (has(variables.o.metadata.generateName) && variables.o.metadata.generateName.startsWith('sa-')))"
		issuerCheck := "namespaceObject != null && (request.operation == 'DELETE' || !" + generatedAccount + " || (has(namespaceObject.metadata.labels) && 'stego.dev/allocator' in namespaceObject.metadata.labels && namespaceObject.metadata.labels['stego.dev/allocator'].matches('^[0-9a-f]{32}$') && has(variables.o.metadata.name) && variables.o.metadata.name.startsWith('sa-' + namespaceObject.metadata.labels['stego.dev/allocator'] + '-')))"
		guarded = append(guarded, allocationPolicy(base+".account-issuers", []any{allocationRule("", "serviceaccounts")}, selected, variables, []any{validate(issuerCheck, "ServiceAccount issuer must match its namespace allocator")})...)
	}
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
	if len(serviceAccountCases) > 0 {
		rules = append(rules, object{"apiGroups": []string{""}, "resources": []string{"serviceaccounts"}, "verbs": []string{"get", "create", "patch"}})
		rules = append(rules, object{"nonResourceURLs": []string{allocationNamespaceCapability}, "verbs": []string{"get"}})
	}
	if len(networkCases) > 0 {
		rules = append(rules,
			object{"apiGroups": []string{"networking.k8s.io"}, "resources": []string{"networkpolicies"}, "verbs": []string{"get", "patch"}, "resourceNames": []string{"stego-allocation"}},
			object{"apiGroups": []string{"networking.k8s.io"}, "resources": []string{"networkpolicies"}, "verbs": []string{"create", "list"}})
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
