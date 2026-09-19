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

The STEGO variant uses separate allocation and restricted application accounts
for a shared cluster. Hypershell main `dbd8363` now permits configured Sandbox
setup after the complete Gateway workflow passed at source `7f81556`. Namespace,
account, certificate, and admission checks remain active. This result checks
allocation and setup; it does not establish live OpenShell Sandbox execution.
See the [live workflow evidence](https://github.com/jsell-rh/hypershell-stego/blob/dbd8363ed2aeb77483fe2cfb98a31f84e808ac18/acceptance/sandbox-activation-live-evidence.json).

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

## Upstream controller trust decision

On 2026-09-19, the user selected the unchanged upstream Agent Sandbox controller
as trusted cluster infrastructure. Hypershell pins version v0.5.4 at commit
`945016a7b97f46cd2edf8633d6b6a22d5355ecc1`. Its entry point has no namespace-only
mode. Its controller role can change Pods, Services, and storage claims across
the cluster. An optional cache label filter does not restrict API permissions.

The operator owns this installation and its cluster-wide permissions. STEGO
must not build a separate controller entry point for this integration. Common
allocation, permission checks, and telemetry remain in STEGO. Gateway workers,
Sandbox accounts, and application API identities retain their existing
permission limits; they do not receive the external controller's permissions.
Review the upstream installation and its permissions when its pin changes.

This decision resolves the controller trust question. It does not change an
installed controller. The qualified application now permits configured setup
with the existing permission checks. The live Kata test remains deferred.
Native network and allocation checks do not establish live OpenShell Sandbox
execution or VM isolation.
