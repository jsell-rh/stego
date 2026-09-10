# Remaining command validation mismatch

C1 is not complete. The shared semantic gate rejects malformed declarations and
many invalid combinations. Some component checks still run only in `Generate`.
A successful `validate` result therefore does not yet prove that all component
inputs satisfy their contracts.

A probe on 2026-09-10 used compiler
`635dc4636f1d2ffa808b300a668c2eceaae52ad3` and a temporary copy of Hypershell
`0128ef569be258a92906cf6da0ab6d32625b54f4`. It first changed only the gRPC
application's `factory_package` from `internal/grpcapi` to `out/grpcapi`.
A second probe tested each application component in a separate temporary copy.
Each copy changed only that component's factory from `internal/` to `out/`.

| Component | `validate` | `plan` | `apply` |
| --- | --- | --- | --- |
| `cli-application` | Exit 0 | Exit 1: factory inside output | Exit 1: factory inside output |
| `http-application` | Exit 0 | Exit 1: factory inside output | Exit 1: factory inside output |
| `grpc-application` | Exit 0 | Exit 1: factory inside output | Exit 1: factory inside output |

Each successful validation reported no issues. The probes compared output, state,
and dependency hashes before and after the commands. They were unchanged in all
three copies. The temporary copies were removed. This is a command consistency
defect, not evidence that invalid output was written.

`compiler.Validate` calls `validateSource`. `Reconcile` calls the same function
before generators run. However, `grpcapplication.Generator.Generate` checks the
factory path and required peer contracts later. Registry type checks accept the
factory as an ordinary string, so the earlier gate does not detect this case.

The next C1 change must make these component input checks available before
rendering. Validation and generation must share the same check and resolved
context. The regression must require `validate`, `plan`, and `apply` to reject
the input while preserving existing output. Other component-specific checks
must be reviewed before C1 can be marked complete. A patch for this one example
alone will not establish a complete common validation stage.
