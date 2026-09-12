# Namespace allocation

STEGO generates a separate namespace allocator worker when a service declares
`allocation_roles`, `allocation_profiles`, and one worker with
`namespace_allocator: true`. The worker must also declare `kubernetes_api: true`
and the `kubernetes` external endpoint. Its permissions come from the profiles;
it cannot add `kubernetes_permissions`.

The generated `deploy/allocation` package contains the common lifecycle code.
The application supplies the controller adapter. The adapter must read current,
authorized application state before each call. Events are work hints, not
permission to allocate a namespace. Use the generated controller runtime for
bounded work, retries, shutdown, metrics, logs, and traces. Keep deleted records
until allocation cleanup returns complete.

A profile sets these values:

- A name, namespace prefix, and exact suffix length. Patterns cannot overlap.
- An application owner label and manager value.
- Quotas for Pods, CPU, memory, temporary storage, and persistent storage.
- Fixed roles and service-account subjects. A subject belongs to the control
  namespace or to the allocated namespace.

Namespaced roles become unbound ClusterRoles. The allocator grants them through
RoleBindings in allocated namespaces. A declared cluster role can only read
Node or Namespace metadata, or create TokenReviews or SubjectAccessReviews.
Wildcards, role escalation, impersonation, and unrestricted role binding are
rejected. The allocator cannot receive an application role.

An `external_role` is an explicit reference to an operator-managed ClusterRole.
It can receive a namespaced binding only. The operator must control that role
and its future changes. For example, an OpenShift test can reference an approved
SCC role. STEGO does not certify permissions in an external role. Use declared
roles when their rules can be part of the service source.

The allocator identity has no Secret access. It can bind only the exact roles
in the generated declaration. Each installation has a distinct marker derived
from its control namespace and worker name. Namespace ownership includes that
marker, the profile, the application owner, and the manager. The runtime does
not adopt a foreign resource. Each patch and delete uses resource identity and
version checks.

`Ensure` creates the namespace with restricted Pod security. It applies the
quota before it grants access. It reads complete binding snapshots before it
removes stale bindings. A changed role or subject is removed before its new
binding is created. The caller must retry `ErrPending` from a fresh application
observation. Both snapshots have a limit of 64 matching bindings. An incomplete
or larger snapshot stops cleanup without a binding write.

`Delete` removes owned cluster bindings before namespace deletion. It also finds
bindings that are no longer in the current profile. It returns true only after
these bindings and the namespace are absent. It can remove an orphan binding
when the namespace has already gone. Namespaced resources use namespace cleanup.

Profile identity fields are not a migration interface. To change the namespace
pattern, application owner label, manager, or profile name, keep the old profile
until its allocations are removed. An application that moves a resource must
retain its old cleanup target. Removing a profile first makes that target
unavailable to the generated allocator.

## Admission checks

The generated worker manifest installs three fail-closed ValidatingAdmissionPolicies
before its access binding. The policies restrict allocator writes to its fixed
profiles, quotas, roles, subjects, and namespace patterns. Namespace ownership
and restricted Pod security cannot change. Other identities cannot claim the
installation marker on a new namespace. A worker cannot remove the quota or
change reserved allocation bindings. Namespace deletion can still remove them.

A namespaced proof RoleBinding gives the allocator permission to read one named
Lease. No Lease object or credential is created. Before a cluster binding can be
created or changed, admission checks that namespace permission through the
Kubernetes authorizer. Thus a matching namespace name alone does not authorize
a cluster binding. Owned binding deletion does not require the proof, so cleanup
can continue after namespace loss.

The live tests also cover two admission contexts. On the tested server,
namespace data was available during validation, but not during match-condition
evaluation. Collection-delete requests have no resource name. Keep these cases
in the live checks when the policy generator changes.

The operator must install and retain the policies and role definitions. The
allocator cannot change these objects. Policy installation order does not replace
an operator check that the policies are active before the worker starts.
Kubernetes v1.35 is the tested API-server version. The implementation uses
[ValidatingAdmissionPolicy](https://kubernetes.io/docs/reference/access-authn-authz/validating-admission-policy/),
including the namespace object, quantity checks, and authorizer checks.

This feature controls API permissions. It does not supply tenant network policy,
storage isolation, a cluster-wide allocation count limit, or an application
adapter. These remain separate acceptance requirements. Hypershell adoption must
pass the real Gateway workflow before its existing worker permissions are removed.

## Checks

`TestAllocationValidation` checks unsafe declarations. `TestAllocationManifests`
checks stable output and emits a test manifest when
`STEGO_ALLOCATION_ARTIFACTS` and `STEGO_ALLOCATION_NAMESPACE` are set.
`TestGeneratedAllocationRuntime` compiles the actual generated Kubernetes client
and allocator. Its TLS tests cover convergence, ownership, quota failures,
regeneration, bounded snapshots, and deletion order.

The optional `TestClusterAllocationLifecycle` uses a projected allocator token
in a bounded cluster Job. Set `STEGO_ALLOCATION_LIVE=1`, the control namespace,
and an eight-character `STEGO_ALLOCATION_SUFFIX`. Install the generated fixture
policies and roles first. The test creates no workload Pod.

`scripts/check-namespace-allocation.py` checks the installed fixture policies with
real API requests. Supply an explicit `--context`, the dedicated `--namespace`,
and an `--evidence` directory. It checks allowed access, denied cross-namespace
access, forged ownership, wrong bindings, quota changes, and orphan cleanup.
Set `STEGO_ALLOCATION_NEXT=1` for a second manifest that removes cluster access
and replaces the data worker. Pass that file with `--next-manifest` to check
binding cleanup and access changes after regeneration. Run this check after the
runtime Job; it changes the fixture policies. It removes its own fixtures. The caller must remove the test policies, roles,
and control namespace after all checks. Do not run these tests in a namespace
that contains application workloads.
