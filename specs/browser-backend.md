The browser-backend component and browser-service archetype generate a separate
Go browser backend. The user accepted Go and a separate process. The browser
HTTP contracts must stay compatible. The bearer-token API keeps its own process.

STEGO owns login, server-side sessions, token refresh, logout, static asset
serving, and the API proxy. The application owns its pages, domain rules, and
API. The component contains no Hypershell paths, roles, or resource models.
It accepts declared browser routes, asset inputs, an API path prefix, and an
identity roles claim path. Asset inputs are captured by the compiler. Unknown
settings, reserved routes, missing assets, and excessive asset sizes fail
before output is written.

The service uses the compiler's database pool, HTTP server, health checks, and
telemetry. It requires a separate HTTPS API origin and verified HTTPS identity
endpoints. The database retains the accepted verified TLS policy. A schema
migration must run before startup. The backend does not migrate the database.
It must not share its process with a bearer-token API.

Login uses a confidential OIDC client, an authorization code, S256 PKCE, state,
and nonce checks. The callback consumes the pending login once. It checks the
signed identity token, issuer, audience, issue time, expiry, and nonce. It then
creates a new session ID. The browser receives an opaque, host-only, Secure,
HttpOnly cookie. The login cookie uses SameSite=Lax for the identity callback.
The active session cookie uses SameSite=Strict. Tokens stay in encrypted
PostgreSQL records. AES-256-GCM binds each record to the hash of its cookie ID.
The database does not store the raw cookie ID.

Version 1.5.0 adds session-key rotation. `STEGO_BROWSER_SESSION_KEY_FILE` keeps
the same name and private-file access rules. It accepts a single key as padded
standard base64, or this JSON shape:

```json
{"version":1,"keys":["BASE64_WRITE_KEY","BASE64_RETAINED_KEY"]}
```

Replace each placeholder with a random 32-byte key encoded as base64. Do not use
a password as a key. The first key encrypts new records. Every listed key can
decrypt records. The file must contain one to three distinct keys and at most
1,024 bytes. Duplicate fields, unknown fields, other versions, invalid JSON,
and noncanonical base64 fail at startup. Error messages omit key values.
The single-key format permits surrounding white space. Each key inside JSON
must contain only canonical base64. The runtime reads the file at startup;
a file update does not change keys in a running process.

Rotate each write key before it encrypts 2^32 records across all instances.
Count login records, session creation, and completed refresh writes together.
This is the standard AES-GCM random-nonce limit; the record capacity limit does
not count writes over time. The runtime does not enforce a shared lifetime
write count. Set the rotation interval from the total write rate and include
an operating margin. A measured capacity and key-use budget remain required
for production operation.

Use these steps for a planned rotation:

1. Deploy the new runtime to all instances with `[old, new]`. Keep the old key
   first until every instance can read the new key.
2. Deploy `[new, old]` to all instances. Both instance groups can read each
   other's records during this change. New sessions and completed token
   refreshes use the first key. Read-only access does not rewrite records.
3. Stop all writers that use the old key first. Retain the old read key until
   all sessions written by those instances have expired. Active sessions have
   a fixed one-hour life; refresh does not extend it. Pending logins last five
   minutes. Include clock uncertainty in the retention period. Then deploy
   `[new]`. Do not remove the old key early on the assumption that every user
   has made a request.

The JSON file is not compatible with runtimes before 1.5.0. First update those
runtimes with the existing single-key file. A rollback after the write-key
switch must retain both read keys. Removal of a compromised key can end sessions
that still require that key; this is different from a planned rotation.

The encrypted record format stays unchanged. Each read makes at most three
AES-GCM attempts. Every attempt checks the same authenticated cookie hash.
A retained key cannot bypass expiry, refresh ownership, or logout checks.
The design uses the standard library's
[authenticated encryption contract](https://pkg.go.dev/crypto/cipher#AEAD) and
[strict base64 decoder](https://pkg.go.dev/encoding/base64#Encoding.Strict).
It also checks canonical encoding, because the base64 decoder permits line
breaks even in strict mode.

The bounded PostgreSQL cluster check passed on 2026-09-11. The generated
runtime suite passed under race detection in 6.935 seconds. It checked old
ciphertext, mixed write keys, the third read key, key removal, cookie binding,
tampered records, expiry, logout during refresh, and backend replacement with
an existing browser session. Invalid key files failed before database or
provider access. The generator, shared HTTP client, and registry checks passed
in 62.817, 2.577, and 2.218 seconds. The runtime source matches the frozen archive
in `/tmp/stego-browser-runtime-8j7qeiye`. The Job reached `Complete`.
The Hypershell deployment check is a separate acceptance gate.

Mutations require an exact Origin match and a session-bound CSRF token.
Requests cannot supply API bearer credentials. The proxy sends a fixed set of
headers to one API origin. It does not send browser cookies or return upstream
cookies. Unknown browser routes return 404. Static content has a restrictive
content security policy. It does not permit inline scripts or inline styles.

A database state transition permits one token refresh across service instances.
A second caller receives 503 with Retry-After while refresh is in progress.
The backend does not retry an uncertain token rotation. A lost response or an
old refresh claim requires a new login. Logout removes the stored session
before token revocation. A refresh completion cannot restore a removed session.
A database failure reports temporary unavailability; it does not report a
successful logout.

The initial limits are 16 concurrent requests per process, a 20-second request
context, a 1 MiB request body, and a 4 MiB API response body. A pending login
lasts five minutes. An active session lasts one hour. The shared store permits
at most 1,000 pending logins and 10,000 total records. A bounded cleanup query
removes expired records each minute. These are resource limits, not measured
capacity guarantees.

The first bounded cluster check passed with race detection. Generated runtime
tests took 2.546 seconds. The generator package took 58.511 seconds, including
dependency setup and compilation. The check used PostgreSQL and an HTTPS OIDC
fixture with signed tokens. It checked login, code replay, rejected identity
claims, encrypted storage, denied API requests, backend instance replacement, refresh across two
backend instances, and logout during refresh. Its Job completed. The source
and result records are in `/tmp/stego-browser-runtime-73kjo8cp` on the test
workstation. These results apply to that frozen source snapshot.

The service assembly check also passed. It compiled the generated main
function with the database, health, and telemetry components. The final source
check added external-link access, cache responses, database failure, and the
pending login limit. Its runtime tests passed in 2.780 seconds. The generator,
shared HTTP client, and registry packages passed under race detection. These
results are in `/tmp/stego-browser-runtime-gn_86o_n`. The final source archive
SHA-256 is `77ae069d8124e8544aa33ed739c2c1e2b1a8c08d448c747945f32085fda96cc5`.
The backend files in the checkout match that source archive.

Exported browser telemetry evidence, session key rotation, ingress deployment,
browser-enforced cookie checks, and measured capacity remain open.
Static assets currently require cache revalidation. Streaming API responses
and a complete browser UI are not delivered by this prototype. No Playwright
or workstation load test is used.

The second cluster Job also completed. Its application adoption check generated
Hypershell twice and verified all 129 generated, state, and dependency hashes
against the post-test output and checkout. The input-manifest race test passed
in 1.053 seconds. Hypershell commit `cf21ebd` adopts the common transport
correction. It does not select this browser component. Backend instance
replacement in these tests is not an operating-system process restart test.
Both test namespaces were removed. Cleanup was verified through the cluster
API. No test workload from these checks remains in the cluster.

The real Hypershell browser protocol test passed under race detection in
21.98 seconds (23.027 seconds for the acceptance package). It used a real
Keycloak provider, PostgreSQL, and separate generated API and console processes.
It checked login, Gateway creation with its owner grant, IDs and API shapes,
filtered lists, denied requests, REST and gRPC retrieval, and event delivery.
It stopped and restarted the console process, then checked the stored session,
actual token renewal, and logout. All 23 console output, state, and dependency
hashes matched before and after the test. The generator and registry race
checks passed in 63.284 and 2.268 seconds. The frozen sources and results are in
`/tmp/stego-browser-workflow-pwsizhdc`.

The application test exposed a missing tracing binding in the new archetype.
The next attempt exposed a nil header map in the test client. Both were fixed
before the final fresh test. A review also found that different ports do not
isolate cookies. The runtime now requires distinct cookie hosts for the console,
API, issuer, and discovered authorization endpoint relative to the console.
Case and a final DNS dot do not bypass this check. HTTPS host names must use
ASCII; international names can use punycode. This follows the
[cookie port isolation limit](https://www.rfc-editor.org/rfc/rfc6265#section-8.5).
The final test used separate loopback IP addresses and checked that console
cookies were absent from API and identity-provider requests.

This is application protocol evidence. The page remains a scaffold. A complete
Gateway UI, real browser cookie enforcement, production deployment, key rotation,
and measured capacity still need tests. The browser archetype does not yet
generate Kubernetes deployment resources.

Hypershell commit `beeea2a` adopts compiler `a6e7656` and the separate console
module. Two runs of the pinned generation script produced the same 152 output,
state, and dependency hashes across both services. Those hashes also match the
application checkout. The input-manifest race test passed in 1.060 seconds.
The console runtime files match the protocol-tested files; only the compiler
state changed during adoption. The cluster Job completed. Its namespace was
removed, and the cluster API confirmed removal. Full CI for the new revisions
remains a separate check.

Reference UI inspection found two missing host contracts: the fixed API
reauthentication response and a usable GET sign-out link. The next change adds
the 401 response and a GET confirmation page with a CSRF-protected POST form.
It adds an explicit identity-provider sign-out scope. The common runtime checks passed under race detection in 8.978 seconds;
the registry checks passed in 2.347 seconds. The extended Gateway test passed
in 15.41 seconds (16.459 seconds for the package). It verified the 401 response,
access before sign-out confirmation, provider sign-out, and a password prompt
on the next login. All 23 console hashes matched repeated generation.
The records are in `/tmp/stego-browser-logout-zw9kt6cp`. The existing UI still needs migration; these corrections
do not establish complete browser compatibility.

The first check rejected an existing empty JSON logout request. The parser was
corrected. The application check then exposed a readiness race in its fixture
and an incorrect assumption that the provider always shows a confirmation form.
The fixture now waits for a health sample within five seconds and accepts a
fixed return from a provider whose session has already ended. These failed
results remain in the test records. The later workflow passed in the same Pod
after the private identity realm was reset. This does not change the failed
status of the original Job. A fresh check with the published compiler is recorded below.

Hypershell `a29934b` adopts compiler `e7febe3` and selects provider sign-out.
The fresh Gateway browser workflow passed in 23.15 seconds (24.198 seconds for
the package). The input-manifest race test passed in 1.059 seconds. All 152
generated, state, and dependency hashes matched both generation passes and the
checkout. The tested application source matches the checkout. The fresh Job
completed. Its records are in `/tmp/stego-browser-logout-pin-n9t00fim`. Both
test namespaces were removed, and removal was verified through the cluster API.
Full compiler CI passed for `e7febe3`. Full application CI remains a separate
check. The complete UI and production browser requirements remain open.

The user confirmed on 2026-09-11 that console sign-out must end both the console
and identity-provider sessions, with a confirmation page. This is now an
explicit requirement. The existing `identity_provider` setting and Keycloak
acceptance test implement this choice. Opening the confirmation page must not
end either session.
