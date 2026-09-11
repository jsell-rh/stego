# TypeScript browser client contract

The `typescript-sdk` component supplies the common browser transport, request
methods, models, and runtime checks. The application supplies the captured
OpenAPI contract and its domain UI. The component has no Hypershell paths,
resource names, roles, or UI code. Both SDK generators use the same captured
OpenAPI loader. Generation does not fetch contract references from the network.

The output is an ES module with TypeScript declarations and package metadata.
It has no npm runtime dependency. A method name comes from `operationId`. The
input contains declared path and query arguments and an optional or required
JSON body. A result contains the HTTP status, validated body, and ETag.
Request and response model types apply `readOnly` and `writeOnly` separately.
Static types do not enforce every schema constraint. Runtime checks also apply.

The client uses the current HTTPS browser origin. It does not accept an API
origin, bearer token, cookie, or custom header. Fetch uses same-origin mode and
credentials, no cache, no referrer, and rejects redirects. Before a mutation,
it reads `/auth/session` and supplies the session CSRF token. It does not retain
a CSRF cache. Public session results exclude that token. `logout()` performs
console-only sign-out. Provider sign-out uses the backend confirmation page.

Each operation, including the session request, has a 20-second deadline.
Callers can cancel with an AbortSignal. A module permits at most 16 concurrent
operations, with no queue or automatic retry. A request body is limited to
1 MiB and a response body to 4 MiB. URLs are limited to 8192 characters.
Input and response checks bound JSON depth, node count, and schema work.

JSON parsing rejects duplicate keys, malformed Unicode, non-finite numbers,
and unsafe integers. Integer-valued JSON numbers must fit the JavaScript safe
integer range, including `int64` fields and unknown fields. Decimal values that
round to an integer also fail. This client does not supply BigInt wire support.
Errors contain fixed transport codes, status numbers, and an optional declared
public API code. They exclude response bodies,
credentials, URLs, and underlying exception messages. The runtime emits no logs.

Supported input is a bounded subset of OpenAPI 3.0. It includes JSON operations,
scalar path and query arguments, explicit success codes, object and array
models, references, composition, scalar enums, nullable values, and declared
size and numeric bounds. Format checks support date-time, URI, numeric formats,
and password strings. URI validation is not permission to navigate to a URI.
Transport destinations always use the fixed browser origin.

The compiler rejects unsupported constraints, recursive models, callbacks,
header or cookie parameters, nullable parameters, and non-JSON media types.
Typed constraints require an explicit matching type. The shared loader bounds
captured files and schema expansion. Unsupported schema features require a
compiler change and tests. They must not be silently removed from the contract.

The common test contract uses Records. Tests compile the declarations with
TypeScript 6.0.3 and run the module with Node.js 24.18.1. They cover request
shapes, CSRF, cancellation, concurrency, error privacy, JSON and schema checks,
prototype keys, and request/response field direction. CI installs the locked
test compiler with npm scripts disabled. Existing Go SDK and registry race
checks also passed after extraction of the shared OpenAPI loader.

The Hypershell protocol test runs the generated module against the real Go
browser backend, API, PostgreSQL, and Keycloak. A Node fixture supplies the
ambient browser cookies and Origin headers. This checks the protocol. It does
not prove a browser's cookie policy or rendered page behavior. The module
creates, retrieves, updates, and lists a Gateway. A second user receives denied
access and a filtered empty list. The Go fixture checks the owner grant and
delivers the event. The full test also covers REST, gRPC, process restart,
renewal, and provider sign-out. Repeated generation must retain all file hashes.

The first application check passed in 20.50 seconds, with the package completing
in 21.552 seconds. Review then added separate request model types and stricter
schema checks. The common race and TypeScript checks passed again in 3.438
seconds. The final pinned application check is recorded with adoption.

This is not the complete web console. Integration with the reference React UI,
asset builds, browser telemetry, rendered browser acceptance, production
deployment, credential rotation, and capacity measurements remain open.

Hypershell `f322996` adopts compiler `cc35051`. The fresh pinned Gateway workflow
passed in 20.79 seconds (21.831 seconds for the package). The input-manifest
race test passed in 1.057 seconds. All 155 generated, state, and dependency
hashes match both generation passes and the checkout. Both input manifests and
the tested application source match the checkout. The Job completed. Source
and result records are in `/tmp/stego-ts-sdk-pin-iw1a51yw`. The initial records
are in `/tmp/stego-ts-sdk-_cqhke3j`. Both namespaces were removed, and the
cluster API confirmed removal.

Full compiler CI passed for `cc35051`. The added in-flight cancellation and
whole-operation deadline tests passed in the cluster, with ten Node runtime
tests and the TypeScript check. The package completed in 3.383 seconds. These
tests are in `4213d3b` and do not change generated output. Full application CI
and the remaining UI and production requirements remain separate gates.

Version 1.1 adds explicit login navigation and declared public API error codes.
The compiler bounds and validates `error_codes`. The runtime exposes a code
only when it matches this declaration. It never exposes the reason, operation
ID, or an unknown code from an error response. Login uses the captured HTTPS
origin and `/auth/login`, with a bounded local path and query. URL fragments
are omitted because the backend rejects them. Applications decide
when to call it. Provider sign-out still uses the confirmation page.

The Hypershell React UI passed its application and test type checks against
this version. All 67 console tests and 164 domain UI tests passed in a bounded
jshell Job. These checks do not prove browser rendering or the production
Content Security Policy. Those application gates remain open.
