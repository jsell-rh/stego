# Allocation account checks

Source `2587af50528e18ad243fa9b33f22eaf92f8301bc` passed
[focused run 35223389996](https://github.com/jsell-rh/stego/actions/runs/35223389996),
job `105208834531`. Independent checks matched the exact source archive and
confirmed these six generated runtime tests:

- Fixed account-name vector and distinct owner and installation identities
- Quota before account and binding creation
- Rejection of changed namespace account annotations before writes
- Different account names after namespace reuse
- One owner selected during concurrent allocation
- No account or permission grant after quota failure

The generated runtime used a verified TLS API fixture. This fixture does not
run Kubernetes admission or RBAC. The namespace reuse result proves the
generated object names, not a live authorization decision.

Declaration validation and repeated generation also passed. The two rendered
installations each contain six admission policies with `failurePolicy: Fail`.
Independent inspection confirmed the namespace reservation, generated-name
guards, allocator capability, and installation-specific account-name checks.

| Record | SHA-256 |
| --- | --- |
| Source archive | `434df221fd3d1ed9a042114fe29eb52dc4daebdf8749575667b8cbb17c524f83` |
| Outer test log | `5fd4801fd4eaad8face25c12bfbdee3827f9ac0293cc94e55bb03fc91165375c` |
| Generated runtime log | `2e525af6e896088566679866312c0d73c3e7730f87d07b1b51d654cc34ecf4bd` |
| Primary manifest | `dda67b0c3f7c06fd3c8e50fb15906b0c051e1d50910e7f748afe094e49327c86` |
| Peer manifest | `866c12b46ace6c0fea89f098ee68bc987cd0c93867303a95baa7bed2de013051` |

The first workflow attempt, `35223217528` at `1eefeaf`, failed before a job
started. Its job-level environment used the unavailable `runner` context.
Commit `2628ab1` moved that setting into the running step. The later focused
checks passed. The failed attempt remains a failure.

The full suite passed at source `2587af5` in
[run 35223389766](https://github.com/jsell-rh/stego/actions/runs/35223389766).
Independent checks confirmed all six jobs, all 34 packages in the race suite,
the SQL lifecycle and browser schema checks, and both generated examples.
The compiler job log SHA-256 is
`42667217494bd76fe8b0dc4eca2fedfeb77428f0c9e68c0af6da1811f87f1286`.
The SQL job log SHA-256 is
`8f5a523bfc9428780a6aee2c72cc9c6b32a4ee06b2c48cae72cd45deac52d160`.
A real Kubernetes admission and authorization gate remains required.
Hypershell has not adopted this component version. No compiler or Go test ran
on the developer workstation.

The branch artifact check also passed at the same source in
[run 35223389876](https://github.com/jsell-rh/stego/actions/runs/35223389876),
job `105209282694`. Independent checks matched all 1,206 source files, their
executable flags, the build record, and the compiler bytes. The hosted job
built the compiler twice with separate source trees and caches. Its binary
SHA-256 is `14ae4f89a34b1bb65fab90c8def7e96e45687bfe715b15fe066e9ef17298df6f`.

The branch artifact has no release signature. The signature job was skipped,
as required for a branch build. No release was published. This result does
not replace the live admission gate.

The live test runner and repeated generated runtime checks passed in
[run 35224854351](https://github.com/jsell-rh/stego/actions/runs/35224854351),
job `105213697937`, at source `7f59b08`. Independent checks matched the source
archive and confirmed seven runner safety tests plus all six generated runtime
cases. Both rendered manifests have the same hashes as the qualified generator
source `2587af5`. The job log SHA-256 is
`7329f57b081f860b2b323254d70b09ba2689c3af28eaa1364e314f77f0b43a9e`.

The runner checks authorized recovery for the same owner and denied access for
a different owner or installation while the old cross-namespace grant remains.
It also checks allocator-only account creation and denies forged names and
changed annotations. Those real API checks have not run yet. The runner safety
tests use simulated API responses; they do not prove live admission.


## Earlier live failures

Three live attempts used the same two generated manifests. All 12 policies
passed server type checks. The first attempt could not classify the first
namespace denial. The diagnostic attempt retained the response: Kubernetes
reported `Invalid`, and the peer installation's equivalent namespace policy
issued the denial. The runner now accepts either exact namespace guard name
with the admission-denial message. Transport errors, unrelated policies, and
policy evaluation errors remain failures. Ten runner safety tests passed.

The corrected attempt passed explicit and generated namespace-name denials,
an unrelated generated namespace name, and authorized namespace creation.
It then failed: the operator's dry-run request to create a declared owner
ServiceAccount was accepted. The cause was later traced to missing namespace context in policy match
conditions. See [the source analysis](allocation-policy-match-context.md).
This result remains a failed security gate.

All three attempts created no Pods. Independent reads confirmed removal of
all recorded resources and reserved test namespaces after each attempt.
See the [failure records](allocation-service-accounts-live-failure.json).
The common account change remains unreleased and is not used by Hypershell.
The complete Gateway workflow with the earlier published compiler is a
separate passing result.

## Corrected live admission and authorization

Generator source `845977ff1d98decb9d63de26682b98618177cdf5` and runner source
`0dc886a2a5d93214504bd6ff314cd3c0f51dcf00` passed the dedicated jshell gate.
The runner used the exact two manifests from
[focused run 35228281358](https://github.com/jsell-rh/stego/actions/runs/35228281358).
Independent checks matched the source, manifest bytes, all 16 probe results,
and four distinct namespace UIDs for one reused namespace name.

The gate denied non-allocator account creation, forged owner names, automatic
token mounting, and changed owner annotations. It allowed the original owner
to recover its account after namespace replacement. A different owner and a
new installation could not use the original permission grant. That grant
remained unchanged throughout the check. Both explicit and generated namespace
name restrictions passed. An unrelated generated namespace name remained valid.

All 12 policies passed server type checks. The gate created no Pods.
Independent API reads at `2026-09-17T13:42:47Z` confirmed that all 49 recorded
resource paths and reserved test namespaces were absent after cleanup.

An intermediate run with the corrected manifests stopped after a proper denial
came from another generated owner guard. The test required only one of the two
equivalent guard names. The corrected runner accepts either exact policy and
binding name. It still rejects unrelated errors and policy evaluation failures.
This intermediate run remains failed; its independent cleanup also passed.

The focused generated-runtime check passed all six cases and ten runner safety
checks. The [renderer check](https://github.com/jsell-rh/stego/actions/runs/35228281203)
passed all 11 runtime cases, including workload preservation, authority checks
before scope filtering, and repeated generation. Independent artifact checks
matched all 1,215 source files and compiler bytes from
[two isolated builds](https://github.com/jsell-rh/stego/actions/runs/35228281221).
The branch artifact has no release signature. The full compiler check for
`845977f` was cancelled before a job started. Its successor at `0dc886a` passed all six jobs in
[run 35228525850](https://github.com/jsell-rh/stego/actions/runs/35228525850).
Independent checks confirmed all 34 packages in the race suite, the required
SQL and browser checks, and both generated examples.

The [machine-readable record](allocation-service-accounts-live-evidence.json)
contains the source revisions, hashes, probe names, and check limits. Raw
responses and the cleanup journal remain in persistent operator storage under
`allocation-policy-match-context-20260917/live-owner-guards`.

The live gate used operator impersonation. It proves Kubernetes admission and
RBAC decisions, not authentication with Pod tokens or Sandbox VM isolation.
Hypershell has not adopted this account component. Related allocation support,
a complete consumer rollout, and release qualification remain required.

The extended issuer test runner passed its ten safety checks at `b8f5dd8` in
[run 35229286537](https://github.com/jsell-rh/stego/actions/runs/35229286537).
All six generated runtime cases passed again. Independent inspection matched
the source archive and both manifests to the qualified generator output.
The four new probes isolate the issuer rule from overlapping owner rules.
They have not run on the cluster yet. They must wait for the active Hypershell
CNPG workflow and independent cleanup. No additional cluster test was started.

The complete suite also passed at the extended runner source `b8f5dd8` in
[run 35229286581](https://github.com/jsell-rh/stego/actions/runs/35229286581).
Independent log checks confirmed all six jobs, all 34 compiler race-test
packages, both generated examples, SQL credential preparation, and browser
startup telemetry. The four new live issuer probes still need a separate run.
This result does not qualify those probes or consumer adoption.

## Isolated issuer rule

The extended live gate passed all 20 probes with runner source `b8f5dd8` and
the same qualified manifests from generator source `845977f`. It reused one
namespace name five times, with distinct namespace UIDs. All twelve policies
passed type checks. The gate created no Pods.

The final namespace had another installation marker and no generated account
annotations. It had no overlapping owner-account rule. The issuer rules denied
the old installation's explicit account name and generated-name prefix. A
literal account name and the new installation's own generated name remained
valid. This closes the four pending issuer probes described above.

Independent checks matched the exact runner and manifest bytes, all twenty
responses, namespace and account identities, and removal of all 49 recorded
resource paths. The test lease was released at `2026-09-17T14:38:31Z`. The
[issuer evidence](allocation-service-account-issuer-evidence.json) retains the
exact sources and limits. The earlier failed attempts remain failed records.

These results qualify the common account component for publication. They use
operator impersonation and do not prove Pod token authentication, a third
allocator runtime, Sandbox VM isolation, or Hypershell adoption. The consumer
must use a checked compiler package and pass its complete workflow after
adoption.
