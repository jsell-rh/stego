package kubernetesservice

import "slices"

const allocationServiceAccountPrefix = "stego.dev/service-account-"

// This RBAC capability selects trusted allocator identities. It is not an
// HTTP endpoint. The API server uses it only for an authorization decision.
const allocationNamespaceCapability = "/stego.dev/namespace-allocation"

func allocationHasServiceAccount(p allocationProfile, alias string) bool {
	return slices.Contains(p.ServiceAccounts, alias)
}

func allocationServiceAccountCEL(alias string) string {
	return "namespaceObject.metadata.annotations[" + celString(allocationServiceAccountPrefix+alias) + "]"
}

func allocationServiceAccountAnnotationCEL(config allocationConfiguration, objectName, alias string) string {
	key := celString(allocationServiceAccountPrefix + alias)
	return "(has(" + objectName + ".metadata.annotations) && " + key + " in " + objectName + ".metadata.annotations && " + objectName + ".metadata.annotations[" + key + "].matches(" + celString("^sa-"+allocationMarker(config)+"-[a-z2-7]{26}$") + "))"
}

// Imported aliases need immutable namespace identity, but do not create local
// accounts. Related profiles use the same owner domain, so aliases have the same
// derived name. The namespace in each binding still selects the source account.
func allocationAccountIdentities(p allocationProfile) []string {
	result := append([]string(nil), p.ServiceAccounts...)
	for _, b := range p.Bindings {
		if b.Namespace == "profile" && !slices.Contains(result, b.ServiceAccount) {
			result = append(result, b.ServiceAccount)
		}
	}
	return result
}
