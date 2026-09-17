# RPC rollout policy

The generated RPC Deployment previously always used `RollingUpdate`, with one
replica and one permitted surge Pod. A mutating provider can therefore have two
processes during an upgrade even when its desired replica count is one. The
Hypershell provisioner uses process-local lifecycle gates. Those gates do not
coordinate two provisioner processes.

The common `kubernetes-service` component version 1.19.0 adds an optional
`rollout_strategy` to each `rpc_processes` entry. It accepts only `RollingUpdate`
and `Recreate`. An omitted value keeps the existing rolling policy. `Recreate`
emits no `rollingUpdate` fields. Each RPC Deployment still has one desired
replica. Other resources, permissions, probes, limits, and image inputs do not
change. Unknown values and incorrect types fail generation.

For example:

```yaml
rpc_processes:
  - name: records
    component: grpc-application
    process: records
    rollout_strategy: Recreate
```

A provider that requires serialized external changes should select `Recreate`
until a distributed writer contract is implemented. The RPC endpoint can be
unavailable during an upgrade. Its callers must retain durable intent and use
the existing bounded failure and recovery behavior.

This setting controls Deployment upgrades. It does not establish distributed
exclusion, protect against manual scaling, or fence an old process on an
unreachable node. Kubernetes can create a replacement while a manually deleted
Pod is still terminating. See the official
[Deployment strategy contract](https://kubernetes.io/docs/concepts/workloads/controllers/deployment/#recreate-deployment).
Do not treat one replica or this strategy as a complete single-writer guarantee.

Regression tests check the existing default, both explicit choices, invalid
values and types, stable generation, and equality of all output outside the
selected RPC strategy. Hypershell adoption remains pending.

## Compiler evidence

Source `4fc880bcec6bc2555aca6b5c86bb8b893eaf8254` passed all six jobs in
[compiler run 35214371129](https://github.com/jsell-rh/stego/actions/runs/35214371129).
Independent log checks confirmed both explicit rollout choices, the unchanged
default, and rejection of empty, unknown, incorrect-case, null, boolean, and
object values. The full race suite passed in 34 packages. Compiler log SHA-256:
`7e1a719a16b8a62d4023516fcee22e815d35775d3c46f0679f83afc244c10085`.

[Artifact run 35214370811](https://github.com/jsell-rh/stego/actions/runs/35214370811)
built the compiler twice from separate source trees and caches. Independent
inspection matched all 1,200 source files, the checksum file, and the embedded
revision. Compiler SHA-256:
`380a6e4f2bfcdc0b8dc067b1ac908d34583ef8b69ffeca7e40b5cb461537979d`.
Build record SHA-256:
`f5bbad36b9c72395902cc4777f0bbf8c16381c4054da8ebaba18e8c543947b57`.
No compiler execution or Go test ran on the developer workstation.

The tested source is on remote `main`. Its main signature and test checks must
finish before release qualification. These compiler results do not establish
live application behavior or distributed writer exclusion.
