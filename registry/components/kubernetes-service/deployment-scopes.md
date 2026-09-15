# Separate deployment scopes

The generated renderer accepts `--scope all`, `--scope cluster`, and
`--scope namespace`. The default is `all`, which preserves the previous output.

Use the same image, namespace, worker or RPC selection, file group, and external
endpoint arguments for both partial manifests. The renderer validates the full
input before it selects resources. A partial render cannot bypass a missing
network endpoint or an invalid image digest.

The cluster manifest contains generated ClusterRoles, ClusterRoleBindings,
ValidatingAdmissionPolicies, and ValidatingAdmissionPolicyBindings. An operator
with the required installation rights applies this manifest before workloads
start. The namespace manifest contains generated ServiceAccounts, Services,
Deployments, NetworkPolicies, Roles, and RoleBindings in the selected namespace.
A deployment identity must have only the permissions needed for those objects.

The renderer preserves each object's content and order within its selected
scope. Together, the two manifests contain the same objects as the full
manifest. An empty scope produces an empty Kubernetes List. A new resource kind
or API version requires an explicit scope in the renderer. A resource with the
wrong namespace stops rendering before any output is written.

This option separates generated manifests. It does not grant access, create a
namespace, install a cluster policy, or prove that a deployment identity is
restricted. The installation must set and test RBAC and admission controls.
A CI job must not receive the operator credential. Keep the cluster manifest
under operator control and check that it matches the intended release before
deploying the namespace manifest.

The generated scope tests and full compiler race suite passed at `2097bea` in
[CI run 34927554750](https://github.com/jsell-rh/stego/actions/runs/34927554750).
They cover HTTP, RPC, and namespace allocator manifests, all emitted resource
kinds, empty lists, exact resource preservation, invalid scopes, unknown kinds,
wrong namespaces, and full-input validation. Restricted application CI still
requires installation and live access checks.

Hypershell adopted this renderer in `5adf4a2`. Its bounded check covered all seven
deployment targets. Default output matched the prior release byte for byte;
partial output preserved every object and permission. Repeated generation left
228 output, state, and dependency files unchanged. See
[the application evidence](https://github.com/jsell-rh/hypershell-stego/blob/5adf4a2486bb48b84b2a7fda1a50af74fc203f04/acceptance/deployment-scopes.json).
The workload runner still needs to separate operator installation from CI.
