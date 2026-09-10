STEGO records selected Go build metadata in `last_applied.compiler_build`.
`stego version --json` reports the same fields with the compiler version label.
The existing `stego version` text form remains available.

The record contains the Go version, target OS and architecture, main module
version, VCS type, source revision, and source state. Source state is `clean`,
`modified`, or `unknown`. A missing VCS record does not mean clean source.
An old state file can omit `compiler_build`. Its next apply adds the record.
A compiler record change causes a state update. Apply rejects a plan whose
public state has a different compiler record from the running process.

CLI component 1.3.0 supplies an optional `Application.VersionCommand` setting.
When enabled, `version` returns JSON with separate `application` and `compiler`
records. The compiler record is fixed during generation. The application record
comes from the CLI executable. `CompilerBuild()` also exposes the compiler
record to application code. Application code supplies only the command setting.
The shared metadata reader, report format, and command behavior belong to STEGO.
A custom `version` command remains valid when the setting is disabled.

The command uses no API, login configuration, file scan, or external process.
It accepts no arguments. It does not export linker arguments, dependency lists,
or local build paths. JSON strings have fixed field names. Selected metadata
values have a 256-byte limit. There is no wall-clock build timestamp.

Build metadata comes from [Go ReadBuildInfo](https://pkg.go.dev/runtime/debug#ReadBuildInfo).
Go can omit source metadata, for example with `-buildvcs=false`. A binary copied
away from its repository keeps its compiled metadata. Building from changed
source records that condition; moving or changing the source after the build
cannot change the executable's report.

These records are diagnostic data. They are not artifact digests, signatures,
or complete input manifests. Two modified builds from the same commit can have
the same record and different behavior. Build flags, toolchain patches, module
replacements, and external tools are not fully identified. A clean source
record does not establish trust. Compiler artifact verification, complete input
manifests, and controlled builds remain required work.

The pinned Hypershell generation script uses a fresh compiler checkout and a
fixed Go toolchain in CI. Build metadata is part of the generated CLI source.
A different compiler revision or Go toolchain can therefore change that source
and state even if the resource implementation is unchanged. Upgrade the compiler
pin and generated state together. Old compiler versions can reject the new
state field because state decoding is strict.

Local validation passed: the full STEGO race suite and `go vet ./...`.
The compiler package took 24.354 seconds. The generated CLI tests took 16.744
seconds, including real clean, modified, and unstamped builds. State tests cover
legacy records, repeat generation, compiler changes, and plan-state substitution.

On Go 1.26.8, Linux amd64, Intel Core Ultra 9 185H, three 200 ms samples of
`Current()` in the test executable took 921.3–1017 ns per call, 1184 bytes,
and 7 allocations. This measures the test executable's metadata only. A larger
executable with more dependency metadata can have a higher read cost. This
measurement does not cover generation, process startup, or JSON encoding.
