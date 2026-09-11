The browser backend is under development. It is not in the component registry
or the command-line generator map. Applications cannot select it yet. The
working design uses a separate Go service. This is a working assumption while
the browser process decision is open.

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

Registry activation, a real Hypershell browser workflow, real Keycloak
integration, exported telemetry evidence, session key rotation, ingress
deployment, browser-enforced cookie checks, and measured capacity remain open.
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
