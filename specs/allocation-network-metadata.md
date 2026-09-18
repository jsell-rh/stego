# Pod network metadata candidate

This change is under test. It is not released or selected by Hypershell.

The Pod policy now checks `pods/status`, as well as Pods and ephemeral
containers. A status write can change metadata. It must not bypass the
annotation, runtime, account, or application checks.

An isolated allocation can select
`pod_network_provider: openshift-ovn-node-identity`. The provider permits two
network output keys: `k8s.ovn.org/pod-networks` and
`k8s.v1.cni.cncf.io/network-status`. It does not permit the network-selection
annotation. Applications cannot list these provider domains in
`pod_annotations`.

A write to either output key requires an UPDATE through the status API. The
Pod must retain its assigned node. The authenticated username must equal
`system:ovn-node:` plus that node for OVN, or `system:multus:` plus that node
for Multus. The request must also have the matching `system:ovn-nodes` or
`system:multus` group. The rule permits an authorized node to add, change, or
remove its own output. Other callers must preserve the key and value.
CREATE requests cannot supply either key.

This profile requires OpenShift OVN with network node identity enabled.
It does not support service-account fallback or another network provider.
The rule grants no RBAC permission and installs no network controller. Normal
Kubernetes authorization and the cluster network identity checks still apply.
No Hypershell name or rule is part of this provider.

The jshell read check found OVN Kubernetes, both output annotation keys on
running DNS Pods, and the node identity group bindings. The node roles permit
Pod status updates. The current generated STEGO policy does not include that
subresource and rejects these keys in later ordinary Pod updates. This is the
application failure that this candidate addresses. A live write check is still
required; source inspection is not proof that a Sandbox can start.

Source contracts are in OpenShift's
[Pod identity checks](https://github.com/openshift/ovn-kubernetes/blob/main/go-controller/pkg/ovnwebhook/podadmission.go)
and [Multus identity configuration](https://github.com/openshift/cluster-network-operator/blob/master/bindata/network/node-identity/self-hosted/node-identity-configmap.yml).
Fixed copies and their source commits are retained under
`~/.local/state/stego/runs/allocation-network-metadata-20260918/`.
