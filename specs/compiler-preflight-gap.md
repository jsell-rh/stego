# Component checks before rendering

C1 is not complete. The shared semantic gate now calls component input checks
before rendering. The factory mismatch below is fixed. The later namespace
probe is also fixed by the [Go package-name contract](go-package-names.md).
The complete component and assembly audit remains open.

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

The compiler now creates one resolved context for each active component after
schema and port validation. It preserves archetype order and resolved overrides.
The context includes default settings, peer namespaces, conventions, source
entities, output paths, and captured declared input files. All component checks
finish before any component renders output. Reconciliation uses those same
contexts and retains input snapshots for the final commit check.

`gen.ContextValidator` supplies the common check. It must not change its context,
write files, or render output. Direct generator calls use the same checks.
The following components now implement the contract:

- CLI, HTTP, and gRPC application bridges: factory paths and required peers.
- gRPC: declared protobuf inputs, imports, syntax, descriptors, and Go mapping.
- PostgreSQL storage: names, fields, constraints, references, and migration mode.
- REST: supported observation policy, collection names, references, and routes.
- JWT authentication: mode, claim paths, and header syntax.
- Kafka, outbox, Kubernetes, PostgreSQL clients, controllers, and search: their
  existing path, dependency, setting, or metadata checks.

This initial change covered 12 components. The later
[health generator](health-probes.md) adds namespace and setting checks through
the same contract. The later [HTTP tracing generator](http-tracing.md) also
checks its namespace, service name, and settings before rendering.
The legacy SSO generator initially had no separate input rejection block to
move. The later Go package-name change adds its namespace check. Its other
configuration and runtime behavior still need the broader security audit.
Component patch versions advance for the changed input-check contract. Valid
runtime source is intended to remain unchanged, apart from compiler build data.

Regression tests verify the three command entry points, unchanged existing
output and state, malformed protobufs, missing inputs, and a failure in a later
component before any rendering. Another test changes a declared file during
rendering. The next component still receives the bytes checked before rendering,
and the changed filesystem input prevents a usable plan. Tests for individual
components require direct generation and preflight to reject the same input.

A probe with Hypershell `d0ad397d1d12fbfc99fffabb2bc2fdc280cbc8d5` repeats all
three factory cases. All three commands now return exit 1. Output, state, and
dependency hashes remain unchanged. Valid validation and planning also pass.

A local timing check used ten calls per command after one warm-up call. The
previous compiler was `635dc4636f1d2ffa808b300a668c2eceaae52ad3`. The new compiler
was built from the working change with Go 1.26.8. The same temporary Hypershell
copy was used for both. Other test processes were active. These figures include
process startup and are a development comparison, not a capacity guarantee.

| Command | Previous mean | New mean | New range |
| --- | --- | --- | --- |
| `validate` | 7.669 ms | 22.755 ms | 20.200–27.456 ms |
| `plan` | 131.468 ms | 139.224 ms | 130.452–149.145 ms |

Protobuf validation adds parsing to `validate`; `plan` also parses the captured
bytes for generation. No input is reopened by the protobuf generator. A future
prepared representation can remove repeated parsing if measurements justify it.

The next C1 probe found another defect. In a temporary copy of that Hypershell
commit, changing only the controller `output_namespace` from `controller` to
`bad-name` made `validate` return exit 0. `plan` returned exit 1 while formatting
the package declaration. A canonical filesystem path is not sufficient proof
of a valid generated Go package name. The later [package-name contract](go-package-names.md)
adds shared checks for library names, Go import paths, and resolved protobuf
mappings. Valid nested output paths remain supported. The full component and
assembly audit remains open; these changes alone do not complete C1.

The full `go test -race -count=1 -mod=readonly ./...` run passed with PostgreSQL
required on port 32900. The compiler package passed in 33.742 seconds and the
storage generator package passed in 27.804 seconds. `go vet ./...` passed.
The command and component regressions also passed separately under race detection.

The next assembly audit found two source checks that validation did not use.
Direct compiler calls accepted an absent or invalid module name or Go target.
Distinct slot names, such as `before_create` and `before__create`, could also
produce the same generated variable name. Assembly rejected these inputs later.
Both regression cases failed before the fix.

Project settings, validation, and assembly now share the build-target check.
It checks Go version syntax with the Go version library and the module parser.
Validation also uses the existing assembly check for derived slot names.
Tests require these errors to stop all generators. Command tests require
validate, plan, and apply to reject the slot conflict without changing existing
source, output, state, or dependency files. Valid Go targets remain accepted.
The full compiler race suite passed with PostgreSQL required on port 32902.
Static checks passed. No generator output or component version changed.

The next [dependency target audit](generator-go-targets.md) fixes five missing
component minimums. Generated gRPC code requires Go 1.26.0 because of the shared
security minimum for `golang.org/x/sys`; its direct dependency alone requires
Go 1.25.0. The generated runtime tests now fix their declared target during
dependency resolution and run static checks. The complete target audit remains
open. Target syntax alone does not prove dependency compatibility.
Wiring checks that need generated wiring still occur after rendering. These
remaining checks must become part of the common compiler contract.
