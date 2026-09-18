package kubernetesservice

const ovnPodNetworks = "k8s.ovn.org/pod-networks"
const multusNetworkStatus = "k8s.v1.cni.cncf.io/network-status"

// Network metadata is output from the selected cluster provider. It is not an
// application annotation or a request for an additional network. Only the
// authenticated network identity for the assigned node can change it, through
// the status API. Other callers must preserve both its presence and its value.
func allocationNetworkMetadataCEL(key, prefix, group string) string {
	k := celString(key)
	newHas := "(has(object.metadata.annotations) && " + k + " in object.metadata.annotations)"
	oldHas := "(oldObject != null && has(oldObject.metadata.annotations) && " + k + " in oldObject.metadata.annotations)"
	unchanged := "(" + oldHas + " == " + newHas + " && (!" + oldHas + " || oldObject.metadata.annotations[" + k + "] == object.metadata.annotations[" + k + "]))"
	writer := "(request.operation == 'UPDATE' && request.subResource == 'status' && oldObject != null && has(oldObject.spec.nodeName) && oldObject.spec.nodeName != '' && has(object.spec.nodeName) && object.spec.nodeName == oldObject.spec.nodeName && request.userInfo.username == " + celString(prefix) + " + oldObject.spec.nodeName && " + celString(group) + " in request.userInfo.groups)"
	return "(" + unchanged + " || " + writer + ")"
}
