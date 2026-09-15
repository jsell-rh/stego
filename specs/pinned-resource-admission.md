# Pinned resource admission

The supplied CNPG application workflow passed, but its installer still requires
an operator for every run. The CI identity must not receive cluster-role,
admission-policy, or arbitrary operator installation permissions.

The next CI design separates an operator-owned installation from run-owned
resources. STEGO supplies template admission rules. The installation supplies
the provider templates, role grants, namespace limits, and cleanup adapter.
Hypershell must not add a private copy of this general policy mechanism.

`kubernetes-client` 1.5.1 withdraws the unverified policy renderer from generated
output. The prototype remains under generator test data. Hypershell still pins
compiler `2097bea6e155a889c48514e640507cae26bb3037`; it did not adopt this API.

Four small jshell attempts found defects before application adoption:

| Attempt | Result | Cleanup |
| --- | --- | --- |
| 1 | Kubernetes rejected the namespace UID expression | Automatic cleanup passed |
| 2 | The subresource rule also denied parent Job creation | Automatic cleanup passed |
| 3 | A missing parameter denied creation and blocked namespace cleanup | Manual binding removal and complete cleanup verified |
| 4 | Namespace selectors and cleanup order corrected; parameter lookup still failed after a bounded wait | Automatic cleanup passed |

The third attempt showed that parameter lookup can fail before CEL caller
conditions run. The earlier policies had no namespace selector and could affect
requests outside the test namespace while parameters were absent. The observed
failure was namespace cleanup. No claim is made that unrelated requests were
unaffected. All probe policies, bindings, CRDs, and namespaces are now absent.
The shared Lease is free.

The prototype now places an exact namespace selector on each policy and binding.
It checks an operator-stamped namespace identity and the template UID. It denies
orphan deletion and requires current resource preconditions. The runner revokes
probe rights, removes bindings, and then removes namespaces and templates.
These corrections do not constitute a passing live gate.

The remaining failure reports no policy parameter even though setup created the
template. The [Kubernetes 1.35 policy source](https://github.com/kubernetes/apiserver/blob/release-1.35/pkg/admission/plugin/policy/generic/policy_source.go)
uses shared informers for built-in parameter types and cancels a parameter
context after its last policy is removed. A stopped
shared informer is a possible cause after repeated installation. This is an
unverified hypothesis, not a cluster diagnosis. Do not restart the cluster API
server to make the probe pass. The next design must prove repeated installation,
missing parameters, namespace isolation, and lifetime cleanup.

The [machine-readable evidence](pinned-resource-admission-evidence.json) records
source and result hashes. The [prototype contract](../registry/components/kubernetes-client/pinned-admission.md)
describes the proposed boundary. Full compiler CI for the withdrawal is required.
The earlier compiler checks passed but did not prove live admission behavior.

The CI credential still has a one-hour lifetime. This change does not establish
unattended credential renewal. Long-lived operator credentials and self-renewing
CI bearer tokens are not a substitute for a separate authentication design.
