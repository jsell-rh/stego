# Pod restrictions for allocated namespaces

An allocation profile can require `pod_runtime_class`, `pod_service_account`, or
both. The runtime value is an exact RuntimeClass name. The account value is a
local alias from that profile's `service_accounts`. It cannot select a literal
account name, the default account, or an account imported from another profile.
The normal requirement for a declared account binding still applies.

STEGO generates one ValidatingAdmissionPolicy and one denying binding for each
profile with these settings. The policy selects the immutable allocator and
profile labels. It requires current allocation identity and restricted Pod
security. The account rule compares the Pod with the allocation's sealed account
name and requires explicit `automountServiceAccountToken: false`. The runtime
rule rejects a missing or different RuntimeClass. These checks apply to Pod
creation, Pod updates, and the ephemeral-container update path. Pod deletion
remains available.

Install the generated policy before granting allocator and workload permissions.
Keep its policy and binding outside worker write access. A policy update applies
to subsequent admission requests; it does not stop existing Pods. Plan runtime
changes with that limit. The declaration does not install a RuntimeClass or
prove the isolation of its handler. The operator must verify that runtime.

The fields add no permissions and do not relax the immutable restricted Pod
security level. Profiles without the fields retain their existing output. The
worker does not interpret these settings; enforcement belongs to Kubernetes
admission. See the [admission reference](https://kubernetes.io/docs/reference/access-authn-authz/validating-admission-policy/)
and [RuntimeClass reference](https://kubernetes.io/docs/concepts/containers/runtime-class/).

Generator, hosted, and live admission qualification are pending. This change is
one part of the Sandbox allocation work. It does not enable Sandbox execution,
change workspace setup, or close the deferred live Kata test.
