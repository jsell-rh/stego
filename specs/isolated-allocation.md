# Allocation for an isolated runtime

The common mechanism is in compiler release `1ab6aea`. Hypershell's Sandbox
integration is incomplete. Keep its constructor guard until the complete
allocation path is verified.

An allocation profile defaults to `pod_security: restricted`. An explicit
`pod_security: isolated-runtime` requires all of these settings:

- An exact `pod_runtime_class` name.
- A `pod_service_account` alias from the profile's declared accounts.
- `network_isolation: true`.

The operator must install and verify the runtime handler. A RuntimeClass name
alone does not prove VM isolation. The generated declaration does not install a
handler, select its nodes, or qualify its host configuration. See the
[Kubernetes RuntimeClass reference](https://kubernetes.io/docs/concepts/containers/runtime-class/).

The isolated mode sets the namespace's Pod Security Admission level to
`privileged`. This permits root helpers and declared capabilities that the
standard `restricted` level cannot accept. The generated admission policy still
rejects privileged containers and host access. The namespace label and its
allocation identity are immutable. A profile change cannot convert an existing
restricted namespace to an isolated namespace. Plan such a change as an explicit
allocation replacement. See the
[Pod Security Standards](https://kubernetes.io/docs/concepts/security/pod-security-standards/).

The policy requires the selected runtime and allocated account. It requires an
explicit false value for automatic service-account token mounting. It rejects
host network, PID, and IPC namespaces; host ports; host and inline remote storage;
block devices; mount propagation; dynamic devices and extended resources;
sysctls; Windows settings; unmasked proc storage; explicit privilege escalation;
and ephemeral containers. Root users and the normal container capability defaults
remain available inside the isolated runtime. No Pod setting is changed by STEGO.

Added Linux capabilities require a `pod_capability_grants` entry with an exact
container name, an image pinned by SHA-256, and an explicit capability list.
The container must also drop all default capabilities and explicitly disable
privilege escalation. The declaration permits at most eight grants, with at
most 16 distinct capabilities per grant. These are declaration bounds, not
Gateway capacity limits. Review each grant against the selected runtime and
application. A pinned image does not constrain the command passed to that image.

Pod annotations are denied unless listed in `pod_annotations`. The generated
policy also permits `openshift.io/scc` and
`security.openshift.io/validated-scc-subject-type`, which OpenShift sets during
admission. See the [OpenShift admission source](https://github.com/openshift/apiserver-library-go/blob/master/pkg/securitycontextconstraints/sccadmission/admission.go).
OpenShift also emits `seccomp.security.alpha.kubernetes.io/pod`. The policy
permits this key only with `runtime/default` and a matching structured
`RuntimeDefault` seccomp profile. It rejects unconfined and local-profile values
in that legacy key. See the [OpenShift SCC provider source](https://github.com/openshift/apiserver-library-go/blob/master/pkg/securitycontextconstraints/sccmatching/provider.go).
Application annotation keys must be qualified names. The compiler rejects
reserved Kubernetes, OpenShift, and Kata settings. The operator must review
allowed application annotations against the cluster's admission services.
PersistentVolumeClaims use operator-controlled storage; this policy does not
inspect the storage driver or prove volume isolation.

The trusted allocator still creates accounts, resource limits, network policy,
and exact role bindings. The Pod policy adds no permission to change admission
policy or RuntimeClasses. Account bindings can use an existing operator role,
including an OpenShift SCC role, only through the declared allocation binding.
Application workers do not receive cluster-policy write access.

Hypershell must retain its OpenShell-specific workload rules, selected images,
and placement policy. Those declarations belong above these common controls.
The upstream workspace-copy user and socket volume stay unchanged. The live
Kata test remains deferred; an admission dry-run cannot replace that test.

## Qualification

The first CI attempt at `8f27297` passed declaration validation and manifest
rendering but failed generated runtime compilation. A namespace check had also
been added to an account lookup. Commit `a74c401` removes that erroneous check.
The failed result is retained. Corrected-source CI passed. The first cluster check type-checked all 16
policies, then rejected the positive Pod because its annotation list did not
include OpenShift's SCC subject-type annotation. The corrected policy permits
that exact key. A new cluster check remains pending. No result here establishes live Sandbox support.

At `5cb252d`, the [focused CI run](https://github.com/jsell-rh/stego/actions/runs/35344821570)
passed all four generator checks, 29 invalid-input cases, 20 runner safety
checks, and the generated allocator restart check. Independent inspection
verified the source archive and both rendered installations. Each installation
has eight admission policies; the isolated Pod policy has 14 validations.

Two cluster attempts with this corrected source stopped on API request timeouts.
The first had type-checked all 16 policies before RuntimeClass creation timed
out. The repeat stopped during policy creation. Neither reached a Pod probe.
Both processes are terminal. Their UID journals and all planned resource names
were checked after cleanup: all 59 paths were absent, including resources whose
create requests had an uncertain result. The shared test lease was free.

The full compiler run [35344821534](https://github.com/jsell-rh/stego/actions/runs/35344821534)
passed at `5cb252d`. Independent log inspection confirmed all six jobs, 34
packages with race checks, the PostgreSQL checks, and both generated examples.
The runner update at `36afe5a` passed 31 safety checks and the focused generator
and restart checks. Its generated manifests were unchanged.

The fourth cluster attempt type-checked all 16 policies, then rejected the
positive Pod on the annotation guard. Cleanup removed all 59 planned and
recorded resource paths; the shared lease was released. A separate server
dry-run in the test namespace identified OpenShift's legacy seccomp annotation.
No Pod was stored. The correction permits only its RuntimeDefault pair and
adds live checks for denied legacy values and undeclared application keys.
The correction passed [focused CI](https://github.com/jsell-rh/stego/actions/runs/35346459459)
at `19cb3e2`. Source and artifact checks confirmed four generator checks, 29
invalid-input cases, 31 runner safety checks, and allocator restart.

The fifth cluster attempt at this source passed all 31 admission requests:
five allowed and 26 denied. All 16 policies type-checked. The writer was also
denied account creation, admission-policy changes, and RuntimeClass creation.
The root workspace helper and ordinary socket volume were unchanged. No Pod
was stored and no runtime handler was installed.

The runner exited with failure during cleanup: one policy delete failed and
the policy remained. A separate recovery checked its recorded UID, removed
that exact policy, and independently confirmed all 60 planned and recorded
resource paths absent. It then released the shared lease. The original failure
record is unchanged. Admission passed; cleanup required recovery.

Full compiler run [35346553521](https://github.com/jsell-rh/stego/actions/runs/35346553521)
passed at `1ab6aea`. Independent log inspection confirmed all six jobs, 34
packages in the race suite, PostgreSQL and browser checks, and both examples.
This revision changes only the compiler workflow's manual trigger after
`19cb3e2`; compiler inputs are unchanged. Main now contains this tested revision.
The [main build and signature checks](https://github.com/jsell-rh/stego/actions/runs/35347226987)
also passed. Two separate compiler builds matched. Independent source-inventory
checks and the common installer verified the compiler and build-record
signatures. Four authenticated files were uploaded to a draft and downloaded
for byte comparison before publication.

The [immutable compiler release](https://github.com/jsell-rh/stego/releases/tag/compiler-1ab6aeaad1e9862386c1d8c3d67e124f6814b048)
is published. The common release installer then independently downloaded and
authenticated it. No downloaded compiler was executed on the workstation.
Hypershell main has not adopted this compiler or enabled its Sandbox allocation
path. A separate branch is checking compiler adoption.

The consumer integration must also check annotations added later by network
controllers. Server dry-run proves admission only. It does not check network
attachment, VM startup, or Sandbox execution.

Evidence is retained under
`~/.local/state/stego/runs/isolated-allocation-20260918/`. The two earlier full
runs were canceled after their sources had known compile or admission failures;
they are not passing results. Do not remove the Hypershell guard or qualify
later compiler changes from focused checks alone.
