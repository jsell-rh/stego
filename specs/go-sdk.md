# Generated Go SDK

The `go-sdk` component generates a typed HTTPS client from captured OpenAPI
3.0 files. The application supplies its API contract. STEGO supplies the client
transport, resource limits, safe errors, and optional shared telemetry.

The compiler uses pinned `kin-openapi` and `oapi-codegen` libraries. It does not
run a code generation command or read reference files during rendering. Declare
each reference file in `references`. The compiler captures these files with the
other inputs. Remote references and undeclared files fail generation.

```yaml
overrides:
  go-sdk:
    document: contracts/openapi.yaml
    references:
      - contracts/widgets.yaml
```

Add `go-sdk` to the selected archetype or mixin. Generated files use the `sdk`
namespace by default. The public package exports models, enum constants, and
methods such as `GetWidgetWithResponse`. Its `Options` type accepts `BaseURL`,
`Token`, `CAFile`, and `Timeout`. Call `Close` when the client is no longer needed.
HTTP error responses retain their status and typed body. Transport and decoding
failures return safe errors. A caller can use `errors.Is` to check cancellation
and deadline errors. Inspect both the error and the HTTP status.

The first input profile supports JSON request and response bodies and bearer
authentication. Each operation must have an operation ID. YAML aliases, anchors,
duplicate keys, recursive schemas, and callbacks are unsupported. A boolean
`x-sensitive` annotation is allowed. All body fields are excluded from diagnostics.
Other extensions are unsupported.
The compiler rejects unsupported input. It does not silently remove it. Each
input is at most 1 MiB. At most 64 files, 256 paths, and 512 operations are allowed.
Schema depth and expansion have separate limits. The generated wire file is at
most 8 MiB. Reference names and Go field names must not conflict.

Nullable properties preserve three states: omitted, explicit JSON `null`, and a
value. This includes zero values such as an empty string, `false`, and zero.
The generated model uses the pinned `github.com/oapi-codegen/nullable` v1.1.0
type. Use `field.Set(value)`, `field.SetNull()`, or `field.SetUnspecified()`.
Use `IsSpecified`, `IsNull`, and `Get` to read the state. Responses retain the
state supplied by the server. A caller must still supply required properties
and obey the API constraints.

Component version 2 changes nullable fields from pointers to this type. Callers
that assign pointers must change their source. The version 1 representation
could not preserve an explicit null in an optional property. Hypershell account
descriptions exposed this gap. The common generator now enables the backend's
[nullable type option](https://github.com/oapi-codegen/oapi-codegen/tree/v2.8.0#generating-nullable-types).
No application-specific serialization code is required.

The SDK uses the [shared HTTP transport](http-client-observability.md). It requires verified
TLS 1.3, one origin and path prefix, and a bearer token. It does not expose a raw
client constructor or request editors. Redirects and environment proxy settings
are disabled. The transport does not retry requests. Each client permits at most
16 active calls. Request and response bodies are limited to 1 MiB and 4 MiB.
The default deadline is five seconds. A configured deadline must be between one
and thirty seconds. A shorter caller deadline takes precedence.

The deadline also covers serialization and decoding. Go cannot interrupt a
caller-defined JSON method. Such a method must return promptly. The SDK checks
the deadline again before it returns a result. `Close` cancels transport calls
and waits at most one second for active calls. Telemetry shutdown has its own
three-second limit. A caller-defined JSON method can continue after this wait.

When `otel-tracing` is selected, each client owns a private telemetry runtime.
The shared HTTP transport supplies logs, metrics, trace propagation, and CLIENT
spans. The SDK does not change global OpenTelemetry providers. Without a
collector, local HTTP completion logs remain active. An unavailable collector
must not prevent an API call. HTTP telemetry covers the exchange and body read;
it does not yet measure the complete SDK serialization and decoding operation.

The generated API follows the supplied OpenAPI contract. On 2026-09-11, the
user selected the new typed API with explicit contexts and a bounded HTTPS
client. Preserve HTTP contracts and behavior. Compatibility with the old SDK's
fluent builder API is not required. Existing callers must adopt the typed API.
Resource groups, automatic pagination, token refresh, streaming APIs, and
TypeScript SDK output remain outside this first component. Server validation
remains required.
The Hypershell variant must test typed Gateway operations, access rules, atomic
writes, events, restart, and trace correlation before this slice is accepted.


The compiler pins `kin-openapi` v0.144.0. This version includes fixes for
[GO-2026-6112](https://pkg.go.dev/vuln/GO-2026-6112) and
[GO-2026-6095](https://pkg.go.dev/vuln/GO-2026-6095). STEGO does not import the
affected request-filter package. The dependency still uses the fixed version.

## Compiler acceptance

On 2026-09-11, the jshell job used Go 1.26.8 and PostgreSQL 18.6. The test
container had a one-CPU limit and a 3 GiB memory limit. No local build or
performance test was used.

The generated SDK race tests passed in 13.576 seconds. They compiled and tested
clients with and without telemetry. Compiler regression tests passed in 39.649
seconds. Registry race tests passed in 2.145 seconds. Static checks, module
verification, and `govulncheck` passed. The final scan reported no vulnerabilities.
The captured compiler source archive was
`4c870bc8bb828ff4e3042ab97d6e46a0fcefe887d980edb48b27a82b3774e08f`.
The job resolved and tested the dependency lock files before they were copied
back to the repository. Full remote CI and the pinned Hypershell workflow are
separate checks.

## Nullable field acceptance

On 2026-09-11, an unchanged Hypershell SDK failed the description test: an
explicit null became an omitted field. The initial application archive was
`a0a318e011376afc1afaf293e1a7a952d4d45591c006787344c09cfcc47c123d`.
The correction ran in the bounded `stego-nullable-20260911` jshell job with the
same Go 1.26.8 and PostgreSQL 18.6 images and resource limits stated above.

The generated SDK race package passed in 76.898 seconds. Plain and traced
clients preserved nullable string, boolean, integer, timestamp, and array
fields through HTTPS requests and responses. The tests also checked explicit
state changes through the model methods. Compiler regressions passed in 55.890
seconds, registry tests passed in 2.235 seconds, and SDK static checks passed.
The tested compiler archive was
`bea62bedb6e313724997b060f4307469ad83a725ee1c21edff03207e22c05dc5`.
Later compiler edits changed documentation and component version metadata only.
Pinned Hypershell generation, account lifecycle evidence, and full CI remain
separate checks. These package times include test setup and generated builds;
they do not measure SDK request performance.
