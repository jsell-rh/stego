# Grants between related allocations

This change is under qualification. Keep the shared-cluster Sandbox guard active.
No published compiler or consumer has adopted this change.

A namespace profile can grant a role to an account from another declared profile:

```yaml
service_accounts: [runner]
bindings:
  - role: task-control
    namespace: profile
    subject_profile: gateway
    service_account: gateway
```

The binding lives in the destination allocation. The subject account lives in
`subject_profile`. The two profiles must have the same owner label and namespace
suffix length. The allocator derives the subject namespace from the destination
suffix and the declared source prefix. Callers cannot supply a separate subject
owner or namespace. Both profiles must declare managed accounts, and the source
must declare the selected alias. The source must be a different profile.
Cluster-scoped grants through this relation are rejected.

The common runtime stores imported account names in the destination's immutable
namespace annotations. It does not create those imported accounts locally.
Admission restricts each grant to the declared role, derived namespace, and
recorded account name. The source account retains the common owner and issuer
protection. Account aliases have the same derived name in profiles with the same
owner domain; the namespace remains part of the Kubernetes subject identity.

The allocator creates local limits and accounts before it checks related
accounts. It then checks every related source namespace and account before it
grants workload permissions. Missing or deleting sources remain pending. A
foreign owner, changed account identity, unsafe token setting, or denied read
stops the grant. This order permits two profiles to depend on each other.
The allocator's own proof binding can exist while workload grants are pending.

A grant can remain in the destination while the source namespace is absent.
Recreation for the same owner restores the intended subject identity. A different
owner gets a different account name. Namespace deletion and Kubernetes token
invalidation remain separate lifecycle operations; this change does not claim
instant revocation of an already active application connection.

This supplies a common permission mechanism needed by separate Gateway and
Sandbox namespaces. Hypershell must still declare the domain roles and lifecycle.
Sandbox admission, network policy, workload setup, and live isolation remain
separate requirements. Qualification requires compiler and generated runtime
checks, live admission and denied-request checks, and consumer workflow evidence.
