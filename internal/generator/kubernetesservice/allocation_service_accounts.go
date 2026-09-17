package kubernetesservice

import "slices"

const allocationServiceAccountPrefix = "stego.dev/service-account-"

func allocationHasServiceAccount(p allocationProfile, alias string) bool {
	return slices.Contains(p.ServiceAccounts, alias)
}

func allocationServiceAccountCEL(alias string) string {
	return "namespaceObject.metadata.annotations[" + celString(allocationServiceAccountPrefix+alias) + "]"
}

func allocationServiceAccountAnnotationCEL(objectName, alias string) string {
	key := celString(allocationServiceAccountPrefix + alias)
	return "(has(" + objectName + ".metadata.annotations) && " + key + " in " + objectName + ".metadata.annotations && " + objectName + ".metadata.annotations[" + key + "].matches(" + celString("^"+alias+"-[a-z2-7]{52}$") + "))"
}
