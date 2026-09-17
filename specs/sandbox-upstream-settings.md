# Sandbox setup and upstream settings

This review separates upstream Pod setup, admission, and VM isolation. A Pod
setup change does not by itself provide VM isolation. The live Kata test is
deferred because the available cluster has no suitable runtime class.

## Reference behavior

Reference Hypershell delegates Sandbox Pod creation to OpenShell and the Agent
Sandbox controller. Its control plane grants the Sandbox service account access
to OpenShift's `privileged` security policy. That permission does not force each
Pod to use `privileged: true`. The reference source does not require Kata or
install a custom Sandbox admission webhook.

The STEGO variant proposes stricter controls for a shared cluster. Its current
Sandbox constructor still rejects use until separate allocation is configured.
Keep that guard while the common allocation and admission design is incomplete.
A passing Gateway workflow without Sandbox workloads does not qualify Sandbox
support.

## Settings checked

The pinned Gateway image reports source
`681c9b2d8b9887f230cee4871bdbdbc9a362dfc8` from the OpenDataHub OpenShell fork.
The review also checked NVIDIA OpenShell `v0.0.116` at
`d1155aa70042d3e2ee49dbfa15346b108b7c1d92`.
The [source record](sandbox-upstream-settings-source.json) contains exact file
URLs and hashes. No image or Rust build ran on the developer workstation.

Both versions provide a default runtime class and Sandbox user settings.
A Sandbox request can override the default runtime class. Thus, a default is
not a security boundary. Admission must reject a different runtime class when
the operator requires a specific isolated runtime.

Both versions set the workspace initialization container's user to zero in
code. They also create the sidecar state volume with an empty `emptyDir` object.
The inspected settings do not expose a separate workspace helper user or the
storage medium for that volume. Setting the Sandbox user does not remove the
helper's explicit user setting.

The proposed variant adjusts that helper to use the agent's non-root user and
uses bounded memory storage for the internal socket volume. These adjustments
still need workload evidence. A source comment about non-root copying is not a
successful runtime test. Supplying a pre-existing workspace volume can avoid
helper creation, but changes workspace setup and recovery. Do not select that
path solely to avoid an admission service.

## Implementation direction

Use supported upstream configuration for each setting it can express. Keep
rejection rules separate from setup rules. The former must reject an unsafe
request even if the latter is absent or fails to make the required change.

For the remaining setup changes, compare a small upstream change with a common
STEGO admission component. A common component must scope requests to its owned
allocations, use bounded processing and verified TLS, manage certificate
rotation, and expose the generated health and telemetry interfaces. Failure
must block a Sandbox that needs the security adjustment. It must not change
unrelated Pods.

Kubernetes provides built-in mutation policies as an alternative to a webhook.
The stable feature starts at Kubernetes 1.36. The supported OpenShift release
and feature state must also be checked; the Kubernetes version alone is not a
support claim. See the
[Kubernetes reference](https://kubernetes.io/docs/reference/access-authn-authz/mutating-admission-policy/).

On 2026-09-17, the user asked why a webhook or fork was needed. Neither choice
is an established requirement. The workspace user change reduces privileges;
the memory volume addressed an earlier Kata socket failure. These reasons do
not establish that every supported runtime needs either adjustment. Root inside
a VM is also different from root on the host. The current source review alone
cannot establish the required workspace behavior.

The user then selected the current Hypershell and OpenShell setup. Keep the
upstream workspace user and socket-volume behavior. Do not add a mutation
service, a maintained fork, or a higher Kubernetes baseline for these fields.
The Hypershell prototype's mutation policy and beta feature setup are being
removed. Its previous Kata results remain historical evidence, not current
compatibility evidence for unchanged upstream Pods.

Continue the separate allocation and permission boundary through STEGO. The
reference's declared privileged SCC binding belongs to the selected Sandbox
account in its allocated namespace; it must not grant application workers the
ability to change cluster policy. Pod rejection rules remain separate from
setup changes. A declared runtime restriction is optional and does not select
a runtime or require a mutation service.
Any reusable admission service, certificate lifecycle, validation, and telemetry
belong in STEGO. Hypershell must retain only its OpenShell integration policy.
