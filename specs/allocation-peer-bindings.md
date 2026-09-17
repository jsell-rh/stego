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


The focused check at `9844ad4` passed in
[run 35248772771](https://github.com/jsell-rh/stego/actions/runs/35248772771).
Independent verification matched the source archive, five generator checks,
13 generated runtime tests, and 37 runtime cases. The related manifest contains
six policies. This result checks their generated form, not server type checking
or live admission. See the [focused evidence](allocation-peer-bindings-evidence.json).

The live runner uses `related/manifest.json` and `peer/manifest.json` from that
artifact. It creates no Pods. It checks exact grants, six malformed grant
requests, destination-only read access, immutable imported identity, and source
namespace replacement with another owner and then the original owner. It keeps
the grant identity unchanged across replacement. Cleanup removes both namespace
profiles before their admission guards, with UID preconditions. The ten common
runner safety checks and five related-runner safety checks passed locally.
The cluster check must wait for the active Hypershell tests and verified cleanup.


The refined source-read test passed at `2e7080e` in
[run 35249275310](https://github.com/jsell-rh/stego/actions/runs/35249275310).
Independent checks matched the source archive, all 13 runtime tests and 37 cases,
and all 15 runner safety checks. The three manifests are byte-for-byte equal to
those from `9844ad4`. The denied-read case now targets the related source account
and also requires the control-worker grant to remain absent. Live cluster qualification remains open.


The branch artifact at `2e7080e` passed independent source and byte checks in
[run 35249275381](https://github.com/jsell-rh/stego/actions/runs/35249275381).
The record covers 1,225 source files and two separate hosted builds. No compiler
binary ran locally. This artifact has no release signature and is not published.
The unchanged deployment renderer also passed all 11 runtime checks at `9844ad4`
in [run 35248772755](https://github.com/jsell-rh/stego/actions/runs/35248772755),
including authority checks before scope filtering and repeated generation.
These results do not replace the pending live admission check.

The complete compiler check at `2e7080e` passed in
[run 35249275355](https://github.com/jsell-rh/stego/actions/runs/35249275355).
All six jobs passed. Independent log checks found 34 passing race-test packages,
required PostgreSQL credential and browser checks, and both generated examples.
The component is not published or adopted by the consumer.

The next live check must wait for correction of Hypershell browser run
35248320996. That run failed on a console Pod readiness observation before the
network fault. Its cleanup passed. The earlier passing run does not replace it.

Before publication, test control-namespace account reuse as a separate security
case. Allocator and control-worker bindings use fixed account names. The current
workload account guards do not cover the control namespace. Retain the bindings,
replace that namespace, and test whether an unrelated namespace owner can create
those accounts and obtain their permissions. This is an open test requirement,
not a confirmed exploit. The test must cover both allocator and worker grants.
Any correction must preserve authorized installation and namespace cleanup.
