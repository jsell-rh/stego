# Externally managed workload accounts

This change is under qualification. It has not been released or adopted by
Hypershell.

An allocator can own a workload's ServiceAccount. The deployment renderer must
then reference that account without creating or changing it. Applications must
not rewrite generated Pod identities to achieve this separation.

`deployment.Options.ExistingServiceAccount` selects an existing account in the
target namespace. The command option is `--existing-service-account`. The name
must be a DNS label with at most 63 bytes. An empty value preserves the existing
generated account behavior.

The option requires namespace scope. The complete generated resource list must
contain one ServiceAccount and one Deployment, with matching account references.
Only Service and NetworkPolicy resources can accompany them. The renderer
rejects generated RBAC, admission policies, and other resource types before it
filters by scope. It does not transfer an existing permission grant to a new
identity.

The renderer omits the account and changes only the Pod's serviceAccountName.
Token mounting, security settings, image pull references, probes, owner labels,
and all other workload fields retain their generated values. Rendering performs
no API request. The caller must verify the selected account's ownership and
readiness before it creates the workload. With a STEGO allocator, use its
ServiceAccountName method and allocation readiness checks. Do not copy the
account-name algorithm into an application.

The generated runtime tests check unchanged workload fields, command and typed
API agreement, repeat generation, invalid identities, and rejection of both
namespace and cluster permission grants. This contract does not add related
namespace bindings or remove the shared-cluster Sandbox guard.

## Focused evidence

Source `436147e59eecc96a9a72ab17c401aa7d69d91569` passed
[run 35225786869](https://github.com/jsell-rh/stego/actions/runs/35225786869),
job `105216899026`. Independent checks matched the exact source archive and
confirmed all 11 generated renderer tests. These include the three new account
checks and nine invalid generated-identity cases. Repeat generation passed.
The generated source checks the complete resource list before scope filtering.

The runtime log SHA-256 is
`60521390ec61a88be2fdb1cd81521fd2b25ef854a0c6b95a869087e9acd0a890`.
The generated renderer SHA-256 is
`0ad4ef95c73ecdc617ddfca1520025f48a94ab39ed7ca964613993a4ad15b559`.
This evidence does not qualify a release or a live consumer deployment.


All six jobs in [run 35225786785](https://github.com/jsell-rh/stego/actions/runs/35225786785)
passed at the same source. Independent checks confirmed the 34-package race
suite, SQL lifecycle and browser checks, and both generated examples.
The separate allocation identity run also passed. Its six generated runtime
checks and seven runner safety checks passed. Both manifests are unchanged
from the checked allocation generator source `2587af5`.

The compiler artifact from [run 35225786718](https://github.com/jsell-rh/stego/actions/runs/35225786718)
matched all 1,211 source files and their executable flags. The hosted job built
it twice in separate source trees and caches. Its SHA-256 is
`e158fb8a0397724efa09d6bab9afe5007c9b3bdecc90dcecd31ef9054bc24bb5`.
This branch artifact has no release signature and was not executed locally.
See the [combined evidence record](deployment-account-selection-evidence.json).
The live admission check and consumer adoption remain required.
