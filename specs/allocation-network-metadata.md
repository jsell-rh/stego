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
application failure that this candidate addresses. The live result below checks
these metadata updates. It does not establish Sandbox startup.

Source contracts are in OpenShift's
[Pod identity checks](https://github.com/openshift/ovn-kubernetes/blob/fe886495f2f706c1912d1417daa768c2d78c0597/go-controller/pkg/ovnwebhook/podadmission.go)
and [Multus identity configuration](https://github.com/openshift/cluster-network-operator/blob/61de77e4d4c4f65cd5b3d73e00308bfe837c1066/bindata/network/node-identity/self-hosted/node-identity-configmap.yml).
Fixed copies and their source commits are retained under
`~/.local/state/stego/runs/allocation-network-metadata-20260918/`.

The latest [focused check](https://github.com/jsell-rh/stego/actions/runs/35452543289)
passed at `37fec9d`. It checked 36 network identity cases, 41 runner checks,
configuration rejection, the Pod status rule, and the rendered provider policy.
The common non-annotation checks are unchanged. Source archive and dependency
checks passed. The first attempt failed because a test used an unpinned CEL
import; the failure record is retained.

The first full compiler check failed because an existing test omitted
`pods/status` from its expected resources. Commit `8b5aef7` corrects that test.
The [replacement full check](https://github.com/jsell-rh/stego/actions/runs/35451661881)
passed at `8b5aef7`: all six jobs, 34 packages with the race detector, and both
generated examples. Compiler inputs are unchanged through `37fec9d`. Only the
Python runner, its tests, and evidence records changed after that source. The
latest focused check covers the runner changes.

The first live attempt observed metadata from the real OVN controller and
completed 31 base probes. It failed when `oc patch` requested `GET pods/status`,
which the network node role does not permit. The node role already permits
updates and patches. The probe did not reach admission. This failed attempt is
retained; it is not a passing gate. Independent cleanup verified 65 absent paths
and the shared test Lease was released.

The corrected fixed runner sends direct dry-run updates with the observed Pod
UID and resource version. It does not change the network node roles. Focused CI
passed before the second live attempt started. The second attempt passed at
`37fec9d`: 40 admission probes, with eight allowed and 32 denied. All 16 policies
type-checked, and three permission denials passed. The real OVN controller wrote
its metadata. Assigned OVN and Multus node identity dry runs passed. Workload,
ordinary operator, and other-node metadata changes were rejected by STEGO's
policy. The workload could preserve network output during an ordinary update.

The 20 Pod observations retained the same UID and showed no container execution.
Pod cleanup passed before policy removal. Independent cleanup verified all 65
resource paths were absent, and the shared test Lease was released.
The check stores one Pod with a random, uninstalled runtime handler, an
invalid image registry, resource limits, and a 120-second deadline. It checks
an actual OVN metadata update and uses dry runs for the metadata admission probes.
The runner checks the Pod identity, limits, and container state during the test.
It removes the Pod before its policies. The wrapper checks cleanup before it
releases the shared test Lease. This check does not prove traffic or Kata execution.

Keep the current Hypershell and OpenShell workspace-copy user and socket volume.
This change adds no mutation service and requires no OpenShell fork.

The compiler and bounded metadata admission checks passed. This candidate
is not a release and is not selected by Hypershell. See the
[evidence record](allocation-network-metadata-evidence.json).
