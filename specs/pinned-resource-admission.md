# Pinned resource admission

The supplied CNPG application workflow passed, but its installer still requires
an operator for every run. The CI identity must not receive cluster-role,
admission-policy, or arbitrary operator installation permissions.

The next CI design separates an operator-owned installation from run-owned
resources. STEGO supplies template admission rules. The installation supplies
the provider templates, role grants, namespace limits, and cleanup adapter.
Hypershell must not add a private copy of this general policy mechanism.

`kubernetes-client` 1.5.0 adds the generated `PinnedAdmissionPolicies` helper.
It emits policies and bindings for one named service account. Jobs have fixed
lifetimes. Deployments and custom resources require a matching lifetime Job.
The policies compare operator-owned templates, check operator-stamped namespace identity and template UIDs,
require restricted Pod security, and deny update, scale, and status operations.
No RBAC grant is emitted. See the [component contract](../registry/components/kubernetes-client/pinned-admission.md).

The pure renderer and invalid-authority checks pass in a small local test.
Full compiler CI, live API type checks, positive and negative admission probes,
and the installation-supplied CNPG workflow remain required. The new helper is
not yet evidence of an unattended CNPG CI pass.

The CI credential still has a one-hour lifetime. This change does not establish
unattended credential renewal. Long-lived operator credentials and self-renewing
CI bearer tokens are not a substitute for a separate authentication design.
