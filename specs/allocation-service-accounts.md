# Allocation ServiceAccount identity

The common runtime is published. Hypershell adoption is under qualification.
The shared cluster Sandbox guard remains in place. Cross-namespace binding configuration
and consumer deployment checks are still required before that guard can be
removed. The initial live admission gate passed; see the
[evidence and its limits](allocation-service-accounts-evidence.md).

An allocation profile can declare `service_accounts` with one through eight
aliases. Each alias is a distinct DNS label with at most ten bytes. `default`
is reserved. When this list is present, each binding to an allocated account
must select one of these aliases. Control and external subjects keep their
explicit names.

```yaml
service_accounts: [gateway]
bindings:
  - role: gateway-worker
    service_account: gateway
    namespace: allocated
```

STEGO generates the actual name from an installation tag and an owner tag.
The owner tag input is a JSON array with these strings, in order:

1. `stego-allocation-service-account-v1`
2. The control namespace
3. The allocator ServiceAccount name
4. The profile's owner label key
5. The allocation owner ID
6. The alias

The owner tag is the first 128 bits of SHA-256, encoded as 26 lowercase base32
characters without padding. The installation tag is STEGO's existing allocator
marker: the first 128 bits of SHA-256 over the control namespace, a period, and
the allocator ServiceAccount name, encoded as 32 lowercase hexadecimal digits.
The result is `sa-`, the installation tag, one hyphen, and the owner tag. Its
62 bytes fit the ServiceAccount name limit. The alias is included in the digest
input. No owner ID is placed directly in the name. The name remains stable
across allocator restarts and namespace replacement for the same owner.
Another owner receives a different name. This is a cryptographic identity
derivation; it is not a credential or an authorization decision.

`Allocator.ServiceAccountName` returns this name without an API request.
It validates the profile, namespace, owner, and alias. Callers must still wait
for allocation before they start a workload. The application selects an alias;
it does not copy the name algorithm or create the account.

`Allocator.RequireServiceAccount` checks the namespace identity and the named
account with at most two read-only API requests. It returns the declared name
only when the account has the expected owner, namespace, name, API kind, UID,
resource version, and disabled automatic token mount. Missing or deleting
resources return `ErrPending`. Other failures return an error and no name.
Invalid input fails before an API request. The method does not create or repair
accounts, confirm permission bindings, or check network policy. Continue to use
`RequireNamespace` for the network checks. Account readiness is a point-in-time
observation; the API server still enforces admission when it creates a Pod.

The readiness method is in the
[immutable compiler release for `09efc7c`](https://github.com/jsell-rh/stego/releases/tag/compiler-09efc7c7e588ecf9a2e3b4d7f3b536cac48b81eb).
The exact source passed all six jobs in
[compiler run 35235710355](https://github.com/jsell-rh/stego/actions/runs/35235710355),
including 34 race-test packages. The focused account run passed eight required
runtime tests and 18 readiness cases. The renderer run passed eleven tests.
Both policy manifests match the prior live admission qualification byte for byte.

The [signed build](https://github.com/jsell-rh/stego/actions/runs/35236644870)
passed two isolated builds, signature checks, and four altered-input rejection
cases. The trusted installer verified the local package and the published
release. All four uploaded release assets matched the verified bytes. The
compiler SHA-256 is
`eb87abafbb795753f95d705445264976734c37691897fc2f418268111d6e714e`.
The compiler was not executed on the workstation. Hypershell generation,
installation policy, and complete application checks remain separate gates.

The allocator records the names in reserved namespace annotations. It applies
the quota and network limits before it creates the accounts or their bindings.
Each account has `automountServiceAccountToken: false`. A Pod that needs a
Kubernetes token must explicitly enable its token mount. The generated
bindings use the owner-specific name, including the restricted cluster roles.

Admission rules protect the reserved namespace annotations from replacement.
They permit only the allocator to create a declared owner account. Account
updates must keep the generated name, owner labels, and false automount value.
Kubernetes can maintain image pull references and its unbound default account.
Namespace deletion can remove the accounts. Other actors cannot create an
account with the previous owner's name after namespace reuse.

Creation of namespaces that match these profiles requires a common allocator
RBAC capability: `get` on the non-resource path
`/stego.dev/namespace-allocation`. This path is not a service endpoint. The
admission policy uses the API server's
[authorization check](https://kubernetes.io/docs/reference/using-api/cel/#kubernetes-authorizer-library).
Only trusted allocator identities receive this capability. Each allocator's
own policy still limits its namespace profiles and ownership fields. This
permits multiple registered allocator instances to share a cluster and prevents
ordinary namespace creators from taking a deleted allocation name without its
ownership labels. Existing unrelated namespaces are not changed or deleted.
When allocator instances share a namespace pattern, upgrade all of them before
enabling this reservation. Do not grant the capability to application workers.
The reservation also checks `generateName` prefixes that overlap a declared
prefix. The allocator uses explicit names; it does not use server-generated
namespace or ServiceAccount names.

A separate admission rule checks the installation tag on generated account
names in the reserved namespace patterns. This rule also applies to namespaces
owned by other registered allocators. Those allocators cannot create a former
installation's account name, including through a profile that otherwise uses
literal account names. The local owner rule then checks each declared alias
against its immutable namespace annotation.

The runtime rejects a changed stored name before it writes. Read-only namespace
verification also checks the names. A first upgrade can add a missing annotation
through the allocator; an existing value cannot be replaced. The policies must
be installed before the allocator receives its permissions.

This mechanism avoids a permanent Kubernetes ownership record for each deleted
namespace. Kubernetes role bindings identify a ServiceAccount by namespace and
name. A grant that remains in another namespace therefore cannot identify a
new owner's generated account. See the
[Kubernetes ServiceAccount contract](https://kubernetes.io/docs/concepts/security/service-accounts/).

The allocator remains a trusted control-plane identity. It must use current,
authorized application state. Namespace names and aliases are not authority.
The generated runtime computes the digest; the admission rules check the
declared names and their immutable namespace records. They do not recompute
SHA-256 in CEL. Cluster binding checks retain the declared metadata-only and
identity-review role limits. This does not add cross-process writer fencing.

## Required evidence

Generated runtime tests cover a fixed digest vector, distinct owner and
installation identities, limits before permissions, changed annotations,
namespace reuse, competing owners, and quota failure. The namespace reuse
fixture checks generated object identities. It does not run Kubernetes RBAC.

Qualification must also use a real API server to check the generated CEL,
denied name and annotation changes, namespace deletion and reuse, and access
from the replacement account while an old cross-namespace grant remains.
It must also deny an unmarked replacement created by an ordinary namespace
creator, and permit another registered allocator under its own ownership rules.
That check requires no privileged Pod or Kata runtime. The deferred VM test
does not replace this admission and authorization check.

The runner also checks the issuer rule without overlapping owner-account rules.
The operator creates a replacement namespace with another installation marker
and no generated account annotations. Ordinary literal names and that
installation's generated name must remain valid. The old installation's name
and generated-name prefix must be denied by an exact `account-issuers` policy.
An owner-account policy denial cannot satisfy these two probes. This focused
case does not install or qualify a third allocator runtime.

The live check runner is `scripts/check-allocation-account-identity.py`. Use
the primary and peer manifests from the focused CI artifact. Keep their source
commit and hashes with the result. Run it with an explicit saved context:

```sh
python3 -B scripts/check-allocation-account-identity.py \
  --oc /path/to/oc --context SAVED_CONTEXT \
  --manifest /path/to/manifest.json \
  --peer-manifest /path/to/peer/manifest.json \
  --evidence /persistent/path/new-account-check
```

The runner requires both dedicated control namespaces to be absent. It creates
no Pod and does not install the rendered Deployment. It checks both allocator
installations, namespace reuse, account-name forgery, annotation changes, and
access through a retained cross-namespace grant. Denied admission probes must
name the expected policy. A transport failure is not an access-control pass.

The check has a ten-minute limit and a separate three-minute cleanup limit.
Each API call has a ten-second request limit. The creation journal records
resource UIDs. Cleanup sends UID preconditions and does not delete a replacement
object. A run passes only if both the checks and cleanup pass. Do not run this
check while another live cluster workflow is active.

## Consumer installation checks

Hypershell uses the common compiler on its test branch. At `ad7cbb3`, the hosted
browser workflow passed in run `35238144248`. Three browser instances each
supplied all eight startup stages with correlated logs, traces, and metrics.
Two images were reviewed. Seven image builds, entrypoint and user checks,
231 UI tests, and three console generation checks also passed. The full core
job failed in the namespace-count fixture. The hosted fixture does not run the
real Gateway workload.

The server accepted dry runs for five policy changes and six additions.
Only allocator, worker, and fixture inspector permissions change. Five other
manifests remain equal. The policy has not been installed.

The server check found two fixture defects. The inventory check assumed exactly
19 resources, and the comparison rejected an omitted empty variable list.
Hypershell `67cbac1` replaces the fixed count with exact recorded identities.
It rejects missing, extra, duplicate, or replaced resources. The comparison
accepts absent, null, or empty policy variables but preserves other values.
All 31 focused Python tests passed.

Hypershell `519b178` also checks the actual running Pods. It uses each
Deployment's label selector and a list limit of four. The check requires one
current, ready Pod with the expected namespace, account, explicit token setting,
and UID. A terminating Pod can overlap its replacement but cannot establish
readiness. Both Pod UIDs are included in the workflow evidence.

[Run 35239610431](https://github.com/jsell-rh/hypershell-stego/actions/runs/35239610431)
passed at `519b1781cbb9fda2d4f73fe3220942e53f4b5909`. Independent checks confirmed
all 12 workload account-readiness cases and 36 Pod-evidence cases with race
detection. The test log SHA-256 is
`0a446eb059bbace93a696ce541b5f928134cf35d17f816b2effd1c969918108a`.
The policy artifact matches all 1,441 source files and the published compiler
records. All six manifests are byte-identical to the earlier reviewed plan.
No generated runtime or policy change was needed for these evidence checks.

The complete live consumer workflow remains required. The prepared policy
update requires successful core application checks and an empty test
installation. The operator login and restricted CI credential were refreshed.
The intervening API repeat passed all 52 required tests; independent cleanup
found no remaining API test resources or held lease at 15:22 UTC on 2026-09-17.
No account-policy update has been applied. The live workflow must still prove
account use and recovery with the new runtime and Pod checks.

The core run passed 682 cases but failed
`TestNamespaceCountWorkflowThroughGeneratedWorker`. Its TLS namespace fixture
omitted the declared account annotations. The generated namespace check rejected
that response before the worker became ready. Hypershell `96cbd4d` uses the
common `ServiceAccountName` API to supply both annotations. The runtime check
remains in place.

The next CI run found an incomplete installation fixture before image build.
Hypershell `51567de` adds the namespace, Kubernetes list fields, and qualified
resource names to that fixture. All eight focused Python tests passed. The
incomplete CI run was cancelled; both acceptance jobs completed container
cleanup before a new run started. Its interrupted checks are not passes.

[Run 35241823877](https://github.com/jsell-rh/hypershell-stego/actions/runs/35241823877)
is active at `51567dea428fecbc844dc4b67aea4afde82ffcad` with both fixture fixes.
No generated runtime or policy changed in these fixes. The account policy
remains uninstalled. Independent operator reads at 15:41 UTC on 2026-09-17
found no test workloads, allocations, or held test lease. The prior apply helper
is stale and must not be used. Complete CI checks and the real account workflow
remain required.

At the same `51567de` source, independent checks confirmed seven image builds,
entrypoint and user settings, 231 UI tests, and three console generation checks.
The hosted browser workflow passed in 100.93 seconds. Three browser instances
each supplied all eight startup stages with matching logs, traces, and metrics.
Fixture and service cleanup passed. Both saved browser images were reviewed.
The browser log SHA-256 is
`39da80ebcafa387987c60984acda89cc4f1c05ac85bb495fae3c6294925be977`.
The core job is still active; these results do not qualify the real account
workflow.

A comparison of all 1,441 committed files confirmed that only the two fixture
files changed after the verified `519b178` policy plan. The runtime, compiler
selection, rendering inputs, and all six policy manifests remain equal. The
prepared update requires this comparison, the current full CI result, and
independent core, image, UI, and browser evidence before it can acquire the
cluster test lease. It has not run.
