The CLI component generates a separate command entry point, a command runtime,
and the common HTTPS client. Run or build `<out>/<namespace>/cmd`. The application
factory returns `command.Application` with command names, paths, methods,
request fields, and expected status codes. It supplies no networking code.

The target must be Go 1.25.0 or later. Validation, plan, and apply reject an older
target before generation. This requirement covers file operations and the OIDC dependency.

Set `factory_package` to a module-relative Go package outside generated output.
The package must export `Commands() command.Application`. The compiler validates
that package path. The generated runtime checks the returned definitions before
it reads configuration or sends requests. Definitions with duplicate names,
ambiguous prefixes, invalid routes, or invalid fields are rejected. Command
names have one through four words. The limit is 128 commands and 64 fields per
command. Up to eight named path parameters can bind required flags to whole
route segments. They stay outside request bodies and query strings. Hypershell names and rules do not occur in this component.

Common commands include `login --url URL --token-file FILE [--ca-file FILE]`
and `logout`. Token-file login stores absolute file references in a private
JSON configuration;
it does not copy the token or claim to have verified it with the API. A command
reads the current token file before its request. Logout removes configuration
and retains the externally owned token file. The factory selects the environment
variable and application directory used to locate configuration.

New directories use mode 0700. Configuration and token files must exclude group
and other access. The configuration directory must exclude writes by other
users. Configuration reads reject leaf symlinks. Writes use an open directory
handle, a random exclusive file, file sync, atomic rename, and directory sync.
Failed writes do not replace configuration with partial JSON. Input files must
be regular and bounded; a FIFO cannot block credential or body reads.

HTTPS and certificate verification are required. A supplied CA file selects an
explicit trust pool; omission selects system roots. The shared HTTP client does
not follow redirects or use environment proxy settings. CLI requests have a
15-second deadline. Mutation requests have no application retry loop. After an
uncertain failure, callers must check resource state before they retry a write.

Request fields support string, string-list, integer, and boolean values. A
string-list flag takes a JSON array. `--body FILE` accepts a JSON object and
cannot be combined with field flags. Unknown fields, duplicate keys, invalid
Unicode, invalid types, and unknown or repeated flags are rejected. Input
limits are 256 arguments, 4,096 bytes per argument or string field, 65,536 total
argument bytes, and a 65,536-byte body file. Responses have the shared 4 MiB
limit. JSON validation also limits depth and node count. Output retains exact
JSON numbers. Error responses expose the HTTP status and omit response bodies.

Commands can write to a new file with `--output-file FILE`. The CLI reserves the
file with exclusive creation and mode 0600 before it sends the request. The
parent directory must exist and must not permit writes by other users. Existing
files, symlinks, and devices are rejected. The CLI syncs successful file output
and its directory. An empty reservation is removed after a request or response
validation error. A write or sync error keeps the file for inspection; it can
contain the only copy of a credential. No file failure falls back to stdout.
A successful response with no body creates an empty file when requested.

Commands marked `Sensitive` require `--output-file`. Use `--output-file -` to
select stdout explicitly. Other commands use stdout by default. This output
choice does not change uncertain mutation results: check resource state before
a retry. A process crash can leave an empty or partial private file. The CLI
cannot recover a secret that the server returned only once.

OIDC login uses `login --url URL --issuer-url URL [--client-id ID]`.
The factory can set `OIDCClientID` as the default public client ID. Add
`--issuer-ca-file FILE` for an explicit provider CA. The API and provider have
separate CA settings. Browser login uses an external browser, S256 PKCE, a
random state and nonce, and an HTTP callback on `127.0.0.1` at a random port.
Register the `/callback` path with the provider and permit random loopback
ports. The provider must advertise S256 support. The CLI prints the URL and
tries `xdg-open`; the user can open the URL if that program is absent.

Use `--no-browser` for device login. The provider must support device
authorization. The CLI also sends PKCE proof for device grants when discovery
advertises S256. The CLI prints a verification URL and user code. It follows the
provider polling interval and adds five seconds after each `slow_down` result.
Login has a five-minute limit. Individual remote calls have a 15-second limit.
Transport errors stop login. The CLI requests only the `openid` scope. Password grants and client secrets
are not used.

Discovery and credential endpoints require HTTPS. Discovery can name separate
HTTPS origins, with a limit of eight origins. The supplied issuer is the trust
source for these endpoints. OIDC documents and token responses have a 64 KiB
limit. The generated runtime uses `coreos/go-oidc/v3` for signature verification.
It requires the exact issuer, one matching audience, a valid expiry and issue
time, and RS256, ES256, or PS256 signatures. It also checks the authorized party,
browser nonce, and access-token hash when present. This is a restricted OIDC
profile; providers that use other signing algorithms or multiple audiences
need a separate contract change and tests.

OIDC configuration stores access and refresh tokens in the private file.
Commands refresh near expiry. A persistent private lock file prevents concurrent
CLI processes from using the same refresh token. Lock acquisition has a
15-second limit. Before the token request, the CLI saves a pending state. It
saves the new session before an API request can use the token. A lost response or a failure before replacement leaves a pending state and
requires a new login. If the final directory sync fails, a later read can find
the complete replacement. After a crash, it can instead find the durable
pending state. Neither state permits a retry with the old refresh token. Refresh cannot change the subject, nonce, or
original authentication time. Logout attempts provider token revocation and
then removes local configuration, even if revocation fails. A revocation error
is reported. Provider browser cookies and other applications are outside this
logout operation. The lock file remains in place.

Commands marked for confirmation require `--yes`. Interactive prompts,
stdin bodies, legacy configuration migration, and automatic SDK generation
are not supplied by this version. Current file operations target Linux; other
platforms need build and file-semantics checks before support can be claimed.

Generated tests use a Record application. They check request types, validation,
private files, token rotation, redirects, malformed responses, no response-status
retry, exact number output, and definition limits. The existing HTTP tests cover
transport failure, request completion, cancellation, and bounded timeouts.

Generated OIDC tests use a TLS provider with application-neutral endpoints.
They check signed claims, PKCE, wrong and repeated callbacks, device polling,
concurrent refresh across processes, changed identity, failed session saves,
and revocation. The protocol contracts use [OIDC Core](https://openid.net/specs/openid-connect-core-1_0.html),
[native application OAuth](https://www.rfc-editor.org/rfc/rfc8252), and
[device authorization](https://www.rfc-editor.org/rfc/rfc8628).

The device PKCE extension is tested with [Keycloak](https://github.com/keycloak/keycloak/issues/9710).
The standard device flow is also tested without a PKCE advertisement.

Generated HTTPS clients use the telemetry runtime in their call context.
The [HTTP client contract](../../../specs/http-client-observability.md) defines
completion, propagation, privacy, and resource bounds. Independent CLI entry
points still need runtime integration.
