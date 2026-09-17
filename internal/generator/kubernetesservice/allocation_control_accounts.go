package kubernetesservice

import (
	"slices"
	"strings"
)

// This capability permits installation of fixed control account names. It is
// separate from namespace allocation and is never granted to a runtime worker.
const allocationControlAccountCapability = "/stego.dev/control-account-installation/"

func allocationControlAccountObjects(config allocationConfiguration) []any {
	base := "{{.Namespace}}." + config.Allocator
	accounts := []string{config.Allocator}
	for _, profile := range config.Profiles {
		for _, binding := range profile.Bindings {
			if binding.Namespace == "control" {
				accounts = append(accounts, binding.ServiceAccount)
			}
		}
	}
	slices.Sort(accounts)
	accounts = slices.Compact(accounts)
	for i, account := range accounts {
		accounts[i] = celString(account)
	}
	names := "[" + strings.Join(accounts, ",") + "]"
	capability := allocationControlAccountCapability + "{{.Namespace}}/" + config.Allocator
	reserved := "(has(object.metadata.name) && object.metadata.name in " + names + ") || (has(object.metadata.generateName) && object.metadata.generateName != '' && " + names + ".exists(name, name.startsWith(object.metadata.generateName)))"
	allowed := "object != null && (!(" + reserved + ") || authorizer.path(" + celString(capability) + ").check('get').allowed())"
	items := allocationPolicy(base+".control-accounts", []any{allocationRule("", "serviceaccounts")},
		"request.namespace == '{{.Namespace}}' && request.operation == 'CREATE'", nil,
		[]any{object{"expression": allowed, "message": "Control account names require a trusted installer"}})
	// An operator can bind this role to a trusted deployment identity. Do not
	// create a binding: namespace owners and allocators must not obtain it.
	return append(items, object{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole",
		"metadata": object{"name": base + ".control-account-installer"},
		"rules":    []any{object{"nonResourceURLs": []string{capability}, "verbs": []string{"get"}}}})
}
