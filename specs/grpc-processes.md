Use the `rpc-service` archetype for a service that needs only RPC processes.
Its `grpc-processes` component requires JWT authentication and telemetry, with
no storage adapter. Declare `proto_files` and `processes` under that component.
Use `jwt-auth.mode: verifier`. Build the selected
`out/grpcapi/processes/NAME` executable. The generated project also contains the
standard root service entry point; it is not the RPC process entry point.

The `grpc-application` component can generate separate RPC executables from the
same compiled protobuf contracts and common runtime. Use `processes` to declare
one to sixteen entries. Each entry has a distinct `name` and a project-relative
`factory_package` outside generated output. The component requires
`otel-tracing` when processes are declared.

```yaml
  grpc-application:
    factory_package: internal/api
    processes:
      - name: provisioner
        factory_package: internal/provisioner
    proto_files:
      - path: api/service.proto
        import_path: example/v1/service.proto
```

The domain package must declare this function in `rpc.go`:

```go
func Open(context.Context) (process.Application, error)
```

Import `process` from the generated `grpcapi/process` package. The application
interface has `Register(grpc.ServiceRegistrar) error` and `Close() error` methods.
The compiler captures `rpc.go` as an input. It checks the function signature,
import paths, package, and absence of build constraints before output changes.
Unknown fields, duplicate names, missing source, and missing telemetry fail.
The application must still pass the normal Go build and runtime checks.
Use `process.IdentityFromContext` for verified caller claims. This public
contract works even when the JWT verifier is in a generated internal package.

STEGO generates `grpcapi/processes/NAME/main.go`. The process imports the shared
JWT verifier, RPC transport, and telemetry. It does not construct the primary
API repository or connect to a database. The primary API bridge remains a
separate generated package. A process can use domain dependencies that require
a database, but the process runtime does not require one.

`Open` receives an initialization context with a ten-second deadline. It must
close partial resources before returning an error. A successful return transfers
application ownership to STEGO. Registration runs once before the listener
opens. Registration must not retain the registrar. Cleanup follows RPC shutdown
and runs before telemetry closes. Domain policy and safe public RPC errors remain
application responsibilities.

The process uses the existing `STEGO_AUTH_*`, `STEGO_GRPC_*`, and OTEL settings.
All RPC calls use verified identity and TLS. Stream identity includes token
expiry. The existing transport supplies admission limits, deadlines, error
isolation, and shutdown. Signals cancel the process context. Initialization has
ten seconds. After cancellation or entry into cleanup, the process permits at
most twenty seconds for shutdown and flush. A failed or blocked domain callback
cannot keep the executable alive beyond that limit. These are process bounds;
they do not make domain effects atomic or recover uncertain external writes.

The entry point catches callback panic and `runtime.Goexit` without printing
private values. Failed startup, serving, or cleanup produces a fixed failure
event and a nonzero exit. A blocked stderr writer has at most 100 milliseconds
to emit that final event. Domain code must not log credentials itself.

The process supplies loopback health probes. `STEGO_RPC_MONITOR_ADDR` defaults
to `127.0.0.1:9082`. It accepts only a literal loopback IP and a canonical port
from 1 through 65535. The monitor permits eight connections and has one-second
read, write, and idle limits. It does not expose metrics or application data.

Run the executable with `--stego-probe=live` or `--stego-probe=ready` for an exit
status. A probe has a one-second limit. It does not call `Open`, start STEGO
telemetry, use a proxy, or follow redirects. Go package initializers still run
before `Main`, including for probe commands. Keep domain resource setup in
`Open`; do not put it in package initializers. The probe requires status 200 and the exact
body `ok\n`. Unknown arguments fail. The health server permits only GET.

Liveness is available during initialization. Readiness requires completed
registration and a bound TLS listener. Both fail when shutdown starts. Neither
checks an external dependency. The process exits if its monitor cannot bind or
stops unexpectedly. `kubernetes-service.rpc_processes` supplies separate
Deployment targets with these probes. See [Kubernetes deployment](kubernetes-service.md).
Production certificate rotation remains required work.

The independent Records fixture uses `grpc-processes` without a storage contract.
It checks compiler rejection, repeated generation,
TLS, verified claims, denied requests, stream expiry, shutdown, process restart,
and resource closure. It exports correlated RPC logs, traces, and metrics to a
TLS collector. Tokens and private errors must be absent from those signals.
It starts no database. It also checks errors, panic,
`runtime.Goexit`, and bounded startup and cleanup. The Hypershell account workflow
is the application gate for replacing its handwritten provisioner entry point.

A command-level check uses the built-in `rpc-service` archetype. It runs validate,
apply, dependency resolution, repeated apply, drift, and a complete project
build. An invalid factory then fails validate, plan, and apply while preserving
generated output and state.

The bounded jshell check on 2026-09-11 used one CPU, 3 GiB of memory, a restricted
container, and a twenty-minute Job limit. It had no database. The final frozen
source is `/tmp/stego-rpc-third-y3djdmx5/source.tar`; its command log and zero exit
record are in that directory. The feature source matches that archive.

The generated process check passed in 21.91 seconds, including an 11.31-second
subprocess test with race detection. The supervisor checks covered startup and
cleanup bounds, cancellation, panic, and `runtime.Goexit`. The command-level
project check passed in 5.55 seconds. Go-version checks passed. The prior source
also passed the full gRPC generator suite in 69.747 seconds and registry tests
in 2.115 seconds. The last source changed the public identity bridge and its
fixture; the affected process and command checks ran again.

The first source stopped at a test-fixture dependency conflict. The second used
the real assembler and passed runtime checks, but its command-level check found
that domain code could not import the built-in verifier's internal package.
The public identity bridge fixes that extension contract. Each command finished
before the next frozen source ran in a separate directory. Results from the
first two sources remain in `/tmp/stego-rpc-process-c1gz72ih` and
`/tmp/stego-rpc-second-g5vxyivn`.

The Pod retained its first command's failed exit record. Its final Job state is
therefore `Failed`; that result was not replaced with the later passing result.
The corrected commands each have their own saved logs and exit result. No
passing Job status is claimed for this checker. The namespace was removed after
evidence collection. Full compiler CI and the Hypershell application gate are
separate checks.

The health and deployment extension passed its bounded jshell checks on
2026-09-11. The first source is `/tmp/stego-rpc-health-38hk1v7s/source.tar`.
Its full RPC generator suite passed in 219.747 seconds. Kubernetes generator
checks passed in 2.582 seconds, registry checks in 2.179 seconds, and the
command-level RPC project check in 6.970 seconds.

The final source, `/tmp/stego-rpc-health-final-vhc0ue8l/source.tar`, adds an
explicit dependency pin for the connection limiter. The generated process check
passed in 32.82 seconds, with race detection, real probes, verified RPC calls,
restart, and OTEL checks. The command-level project test passed in 5.41 seconds.
Compiler input-snapshot and Go-version checks passed. Each source has separate
logs. Both command sequences exited zero. The Job reached `Complete` and its
namespace was removed. Full compiler CI passed for `00b270d`.

Hypershell then passed the rendered Gateway and account workflow with a separate
generated provisioner Deployment. It replaced the provisioner after account
creation, then verified token issuance, revoke, and delete. See the
[application deployment record](https://github.com/jsell-rh/hypershell-stego/blob/863d8a9/acceptance/rpc-deployment.md).
