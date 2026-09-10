The compiler checks each component's `gen.GoVersionRequirement` before any
generator runs. A declared target must support the generated dependencies and
language features. Target syntax alone cannot establish this.

The dependency audit on 2026-09-10 found five missing requirements:

| Component | Minimum target | Reason |
| --- | --- | --- |
| `postgres-adapter` | Go 1.25.0 | The compiler pins `golang.org/x/text` to v0.40.0 for GORM. |
| `postgres-client` | Go 1.25.0 | pgx v5.11.0 requires it. |
| `otel-tracing` | Go 1.26.0 | OTLP/gRPC uses the compiler's x/sys security minimum. |
| `outbox` | Go 1.25.0 | pgx v5.11.0 requires it. |
| `kafka-producer` | Go 1.25.0 | franz-go v1.21.6 requires it. |
| `grpc-application` | Go 1.26.0 | The compiler pins `golang.org/x/sys` to v0.48.0. |

The source evidence is each pinned module's `go.mod`, the generator dependency
declarations, and the compiler's shared requirements in `module.go`. The gRPC
module itself requires Go 1.25.0. The first generated target test failed because
the complete generated dependency graph requires Go 1.26.0. The fix retains
the security pins and raises the declared gRPC minimum.

Compiler regressions first failed for all five missing requirements. They now
require unsupported targets to fail before rendering. They also check accepted
targets at and above each minimum. Command tests use a small REST service to
check validate, plan, and apply. Rejected commands must preserve existing
source, output, state, and dependency files. A supported target must validate
and produce a plan. Unrelated generators do not inherit a global minimum.

The five generated runtime test suites now set their module target from the
component requirement. They use `go mod tidy -go=<minimum>`, which must fail if
a dependency requires a newer target. They then run `go vet` and race tests.
The gRPC test uses the compiler's assembled module, including shared security
minimums. It covers both watch settings and services without cleanup owners.
The storage test covers services with cleanup owners.

The new static check found unreachable cleanup-reader code for a service with
no cleanup owners. The template now emits a direct error return after the
common input and context checks. It emits the database query only when cleanup
owners exist. The gRPC static and runtime tests passed after this correction.

These checks use Go 1.26.8 with the stated module targets. They do not certify
an older compiler release for production use. Continue to use a supported,
patched compiler. The complete target audit remains open: component language
features, application dependency overrides, and assembly wiring still need
complete checks. C1 and C7 are not complete.

The final full `go test -race -count=1 -mod=readonly ./...` run passed with
PostgreSQL required on port 32903. `go vet ./...` passed. The command and target
regressions also passed separately under race detection. No dependency version
was lowered. Component patch versions advance for the changed target contract
and the cleanup-reader correction.
