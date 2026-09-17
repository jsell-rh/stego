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
selected RPC strategy. CI and Hypershell adoption remain pending.
