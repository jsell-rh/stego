# Application Pod rules

This extension is under development. It is not a qualified compiler release.
Hypershell has not adopted it.

An allocation profile can add `pod_validations`. Each entry has a CEL
`expression` and a fixed `message`. A profile must already require a Pod runtime
or allocated account. Rules add to the common admission checks. They cannot
change the policy selector, common checks, variables, failure mode, or binding.
The worker receives no expression code or policy-write permission.

The compiler permits 1 to 16 rules, at most 4096 bytes per expression, and at
most 256 bytes per message. It rejects duplicate expressions, invalid CEL,
non-boolean results, template delimiters, and control characters in messages.
Parser depth, recovery attempts, and expression nodes have bounds. The selected
CEL implementation is [CEL-Go 0.32.0](https://github.com/cel-expr/cel-go/releases/tag/v0.32.0).
Only standard CEL functions are available. This extension does not enable
experimental Kubernetes APIs or CEL libraries.

Rules can use `object`, `oldObject`, `namespaceObject`, and `request`.
`variables.containers` combines normal and init containers. No other policy
variable is exposed by the compiler. Rules must account for absent fields.
A policy evaluation error rejects the request.

The compiler checks syntax and result type. Kubernetes must also check the
rules against its installed Pod schema and cost limits before the installer
grants workload access. Dynamic field access in the compiler is not proof of
that cluster check. Application tests must cover their allowed and denied
requests. The compiler does not infer application intent from a rule that
returns true.

Image selection, credential mounts, and domain storage ownership can use these
additional rules. Keep those decisions in the application declaration. STEGO
continues to provide namespace ownership, runtime and account restrictions,
and the policy installation shape.

## Related network peers

An allocation network peer can use `namespace: profile` with `peer_profile`.
The target must be a different network-isolated profile in the same declaration,
with the same owner-label key and namespace suffix length. The compiler resolves
its namespace prefix. The caller cannot supply a separate peer namespace.

The generated selector requires the exact related namespace name, allocator
identity, target profile, and current owner ID. The Pod selector and port remain
required. The generated admission rule requires all four namespace labels;
workers cannot remove one or replace it with a selector expression. A namespace
recreated for another owner does not match the retained rule.

This is an implementation candidate. Release publication and consumer
integration remain incomplete. A correct policy declaration does not prove
that a cluster network provider has enforced it.

## Qualification record

[Focused CI](https://github.com/jsell-rh/stego/actions/runs/35349083894)
passed at `0f093fb`. The checks cover invalid CEL, credential mounts, unchanged
common guards, related namespace owners, and generated allocator restart.
All 33 runner checks passed. Source archive and artifact checks confirmed the
committed dependencies and manifests from two installations.

The jshell admission check at this source passed 40 probes: seven allowed and
33 denied. All 16 policies passed type checks. The application rule permitted
the helper's credential mount and denied the same mount in the workload and
workspace containers. The related network rule permitted the declared selector
and rejected five changes to its namespace or owner labels. Three additional
permission checks denied account creation, policy changes, and runtime creation.

The check retained the upstream workspace-copy user and ordinary socket volume.
It stored no Pod or network policy from a probe. It installed no runtime handler.
Cleanup completed without recovery. An independent check confirmed all 60
planned and recorded resource paths absent, then released the shared lease.

Two earlier attempts remain recorded as failures. The first stopped on a
resource read before the test runner started. The second encountered a valid
namespace-mode denial from a different generated guard than the test expected.
The revised test accepts either of the two exact guards and rejects unrelated
denials. Both failed attempts completed cleanup before another attempt started.

The [full compiler check](https://github.com/jsell-rh/stego/actions/runs/35348765363)
passed at `3358afe`: all six jobs, both examples, and 34 packages with the race
detector. Only two Python runner files changed between that source and
`0f093fb`; compiler inputs did not change. Later changes only update these records.
This candidate is not released or adopted by Hypershell. Network traffic,
network-controller annotations, and live Kata execution are not proved by these
dry runs. Hypershell must retain its Sandbox constructor guard until its full
allocation and cleanup path is checked. Keep OpenShell's current setup; this
extension adds neither a mutation service nor an OpenShell fork.

See [the result record](allocation-pod-rules-evidence.json). Detailed records and
fixed source copies are under
`~/.local/state/stego/runs/allocation-pod-rules-20260918/`.

## Label presence candidate

A network peer can declare `pod_exists_label` instead of `pod_label` and
`pod_value`. The compiler accepts one label key and rejects combined selectors.
The generated rule requires exactly one `Exists` expression with that key, no
values, and no other Pod selector. Namespace, owner, protocol, and port checks
remain in force. This extension is under qualification and is not released.

The application needs this form because the pinned Agent Sandbox controller
sets a separate tracking-label value for each Sandbox. This is a standard
Kubernetes selector; no OpenShell-specific label is built into STEGO.

The [focused check](https://github.com/jsell-rh/stego/actions/runs/35350547635)
passed at `58d9bd4`. It checked selector validation, CEL evaluation, generated
runtime restart, and all 33 runner checks. The jshell admission check at this
source passed 46 probes: seven allowed and 39 denied. Six new denials covered
an empty Pod selector, another key, `DoesNotExist`, `In` with values, an extra
expression, and extra equality labels. All 16 policies passed type checks.
Three additional permission checks also passed.

Cleanup completed. A separate check confirmed all 60 resource paths absent
before the shared test lease was released. No probe stored a Pod or network
policy. These checks do not prove CNI traffic or Kata execution. The
[full compiler check](https://github.com/jsell-rh/stego/actions/runs/35350549513)
also passed at `58d9bd4`: all six jobs, both generated examples, and 34 packages
with the race detector. Only result records changed after that source. Release
publication and consumer regeneration remain pending. See the
[label presence record](allocation-label-presence-evidence.json).
