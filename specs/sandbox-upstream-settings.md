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

The earlier prototype changed the helper to use the agent's non-root user and
used bounded memory storage for the internal socket volume. The memory volume
addressed a socket failure in the earlier Kata fixture. That result does not
establish that every supported runtime needs these changes.

## Implementation direction

On 2026-09-17, the user selected the current Hypershell and OpenShell setup. Keep the
upstream workspace user and socket-volume behavior. Do not add a mutation
service, a maintained fork, or a higher Kubernetes baseline for these fields.
Hypershell commit `b5c536c` removes the prototype's mutation policy and beta
feature setup. Its previous Kata results remain historical evidence, not current
compatibility evidence for unchanged upstream Pods.

Continue the separate allocation and permission boundary through STEGO. The
reference's declared privileged SCC binding belongs to the selected Sandbox
account in its allocated namespace; it must not grant application workers the
ability to change cluster policy. Pod rejection rules remain separate from
setup changes. A declared runtime restriction is optional and does not select
a runtime or require a mutation service.
Keep common allocation, permission checks, and telemetry in STEGO. Hypershell
must retain only its OpenShell integration policy.
