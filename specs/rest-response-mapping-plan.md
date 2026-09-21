# REST response mapping plan

Status: proposed implementation and acceptance requirements. No REST mapping
generator is accepted by this document. Complete the pending Gateway protobuf
workflow before a further mapping implementation.

## Source review

This review uses STEGO `fd891180d6537c303642a25bdd6c9d33096046e4` and
Hypershell candidate `047f550fca807fba214843ce04d50aa85544f5df`.

STEGO already captures and validates local OpenAPI documents in
`internal/openapicontract/input.go`. It rejects undeclared references, duplicate
YAML keys, aliases, recursive schemas, and excessive input expansion. The Go SDK
uses this loader and a pinned OpenAPI backend. Its public types are aliases for
types in its private `internal/wire` package.

Hypershell still defines REST response structures and field copies in
`internal/httpapi/http.go` and `internal/httpapi/catalog.go`. The catalog covers
ManagedCluster, GatewayRelease, and GatewayNetwork. Gateway conversion first
selects `CurrentObservations`, decodes stored DNS names, and adds the creator
name that the application supplies. Grant responses also require domain data.

The OpenAPI schema alone does not specify every existing response choice.
For example, the common reference schema permits omitted timestamps, kind,
and href. The current server sends these fields. Gateway `namespace` is also
optional in the generated SDK type, but the current server always sends it.
An extraction must preserve these choices for valid domain values.

## Common mechanism and application boundary

Generate server response types from the captured OpenAPI contract. Use the
existing loader and pinned backend through a common compiler helper. Put the
types in a public server contract package that needs no SDK client. Do not make
the server import the SDK private package. Keep current SDK public names and
behavior compatible when the common helper is introduced.

Declare response mappings against the selected model provider and schema.
Reuse the checked model field contract used by protobuf mapping. Target names
refer to JSON properties from OpenAPI, not guessed Go field names. Use the
backend's actual Go fields and types. Reject ambiguous composed properties,
name collisions, unsupported types, unknown rules, and incomplete mappings
before output writes. A new schema property must require a mapping decision.

The application selects observations, resolves creator and grant names,
authorizes access, and selects public error responses. Mappers perform no I/O.
They return an owned response or a fixed conversion error without input values.
Application-derived values need a typed input contract. Do not add reflection,
arbitrary Go expressions, callbacks, or database lookups to a mapping rule.
Catalog adoption can precede Gateway and grant adoption, but does not complete
the REST extraction requirement.

The generated transport must continue to encode the complete response before
it writes a successful HTTP status. A conversion failure must not send a
partial object, partial list, or successful status. Preserve field projection,
pagination, and application error policy during adoption.

## Presence and conversion requirements

| Case | Required behavior |
| --- | --- |
| Required source to optional target | Preserve the selected value, including zero, false, and empty string, unless an explicit compatible omission rule applies |
| Optional source absent | Omit the target when the existing contract does; do not emit JSON null by accident |
| Optional source present and empty | Retain presence; copy the value into response-owned storage |
| Explicit nullable property | Preserve absent, null, and value as separate states; reject a source contract that cannot represent the declared mapping |
| Reference kind, href, and timestamps | Preserve the current server presence rules and timestamp precision |
| Gateway creator | Omit an empty creator as the current server does; keep creator resolution in the application |
| Gateway DNS list | Preserve omission for absent and empty lists; validate stored JSON within declared byte and item bounds |
| Empty collection | Preserve the existing empty items array and list metadata; do not change it to null or omit it |
| Integer conversion | Check bounds before conversion; do not truncate or wrap |
| String and timestamp | Check valid encoding and JSON timestamp range before output; do not replace invalid data silently |

Omission rules must be explicit and type checked. Do not infer an omission rule
from a zero value in a stored row. Do not strengthen optional fields in a shared
request schema only to match server response behavior. Preserve public JSON
names and HTTP status contracts.

## Required evidence

1. Compiler tests must prove rejection before writes for bad schemas, missing
   sources, duplicate or uncovered targets, unsafe conversions, unsupported
   presence rules, and schema or Go identifier collisions. Test captured
   references and composed schemas with independent service names.
2. Generated runtime tests must cover all presence states, zero values,
   integer bounds, timestamps with subsecond precision, invalid UTF-8,
   bounded JSON lists, and ownership of pointers and buffers. Compare JSON
   properties and values against explicit expected output. A round trip through
   the same generated type is insufficient evidence.
3. Preserve Go SDK source compatibility and nullable behavior. Verify pinned
   dependencies, deterministic generation, and unchanged unrelated outputs.
   The server contract package must build without an SDK client dependency.
4. Hypershell tests must compare each catalog field and the Gateway response
   shape, including absent, empty, zero, stale-observation, and deleting states.
   Verify filtered lists, projections, denied requests, malformed stored values,
   and unchanged public errors. Keep domain lookups outside generated code.
5. Run the complete bounded Gateway workflow through REST and gRPC, generated
   event delivery, restart, browser rendering, telemetry, and regeneration.
   Verify the exact source, compiler, images, and cleanup. Existing protobuf
   evidence does not prove REST conversion behavior.

Use CI for generated runtime and application suites. Use the single jshell test
slot for the live workflow. This plan does not change the deferred Kata test,
capacity requirements, or the unmet 30-second cleanup target.
