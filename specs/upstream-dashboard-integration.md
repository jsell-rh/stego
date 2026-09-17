# Upstream dashboard integration

The user selected the upstream OpenShell dashboard. STEGO must generate its
common authentication, deployment, and lifecycle support. Hypershell must keep
only Gateway configuration and access rules.

## Current integration state

Hypershell candidate `e8bb965` uses the pinned STEGO registry and compiler
`db75a77da272fe05bbf6fdc3a3d1e0fede298f70`. The API, management console, and
Gateway console keep local application composition and generated output. They
contain no copied common component declarations. STEGO supplies sessions,
confidential browser clients, telemetry, deployment, and shared lifecycle
operations. Hypershell supplies Gateway placement and access rules.

The upstream dashboard build passed at `52db843` in
[35164983445](https://github.com/jsell-rh/hypershell-stego/actions/runs/35164983445).
It uses the common browser client, telemetry, and dynamic style support. Its
editor uses the checked local Monaco source adapter. See
[browser dynamic styles](browser-dynamic-styles.md).

[Live run 35167085887](https://github.com/jsell-rh/hypershell-stego/actions/runs/35167085887)
at `5f87ed6` passed native editor input, layout, syntax colors, selection,
JSON worker behavior, and the invalid JSON marker. No document content-policy
violation occurred in those checks. It also passed SQL isolation and recovery,
controller and workload namespace replacement, viewer membership, filtered
lists, denied writes, both access-removal paths, and dashboard reload with the
retained session. The test then failed after 504.29 seconds: the generated
backend rejected the native sign-out form with HTTP 403. Independent cleanup
passed at `2026-09-17T00:53:49Z` with no test resources or held test lease.
The [application record](https://github.com/jsell-rh/hypershell-stego/blob/5429aae/acceptance/dashboard-signout-live-evidence.json)
keeps the passing recovery evidence separate from that failure.

STEGO now corrects the confirmation document's referrer policy. The
[native browser regression check](browser-logout-origin.md) reproduced the old
`Origin: null` rejection and passed session removal, token revocation, and
provider redirect with the new policy. Strict Origin and CSRF checks remain.
This regression uses a test provider endpoint. The real Keycloak result is
recorded below.

The generated Gateway console at `a19b611` passed
[35168795823](https://github.com/jsell-rh/hypershell-stego/actions/runs/35168795823).
All 129 archived source files and 124 downloaded module output and dependency
files matched. Repeated generation, dependency checks, builds, and the published
image binary and digest passed. Candidate `ca8814f` selects that exact module
and records its final dependency hashes.

[Run 35169043109](https://github.com/jsell-rh/hypershell-stego/actions/runs/35169043109)
at `ca8814f` passed native dashboard confirmation, real Keycloak sign-out, and
a new password login prompt. It also passed authenticated correlation of all
three dashboard telemetry signals, worker telemetry, SQL and namespace recovery,
and rendered service-account creation, token use, revoke, and delete.

The test failed after 550.45 seconds when it created automation identities for
final Gateway deletion. The API returned HTTP 409. The saved response did not
identify the error code. Independent cleanup passed at
`2026-09-17T01:23:23Z`; workloads and allocations were absent and the lease was
free. The [result record](https://github.com/jsell-rh/hypershell-stego/blob/a29edb5/acceptance/dashboard-signout-recovery-evidence.json)
retains these results and their hashes.

Hypershell `a1165b2` adds a fixed error-code record and bounded, read-only
Gateway state inspection on account creation failure. It excludes response
bodies, reason text, and credentials. Its focused privacy test passed.
[Run 35170428686](https://github.com/jsell-rh/hypershell-stego/actions/runs/35170428686)
at `a29edb5` passed creation and real Gateway use of three automation identities.
Main Gateway deletion closed all three accounts, denied token issuance, removed
their provider clients, and recorded their cleanup audits. It removed the
Gateway namespace, SQL state, roles, and keys. The other Gateway and supplied
PostgreSQL server remained available. All 412 repeated and archived generation
hashes matched. The earlier HTTP 409 did not recur; its cause remains unknown.

The test then failed after 791.79 seconds during the remaining Gateway cleanup.
The identity-provider fixture had reached its 600-second Pod deadline, although
the full browser test permits 900 seconds. Independent cleanup passed at
`2026-09-17T01:48:04Z`. See the [main deletion and deadline record](https://github.com/jsell-rh/hypershell-stego/blob/4dd7d21/acceptance/dashboard-main-deletion-evidence.json).
Hypershell `a5d1d16` aligns this fixture with the full test's 900-second limit.
Other identity tests retain 600 seconds. CPU, memory, and outer Job limits are
unchanged. Focused local checks passed.

[Full CI 35168991363](https://github.com/jsell-rh/hypershell-stego/actions/runs/35168991363)
at `ca8814f` passed all 302 expected top-level tests. Four declared live tests
were skipped; CNPG and Sandbox jobs were not selected. Core acceptance took
1536.774 seconds and browser acceptance took 95.119 seconds. The
[full result record](https://github.com/jsell-rh/hypershell-stego/blob/bc64728/acceptance/dashboard-signout-full-evidence.json)
contains the test names and log hash. It does not replace the complete cluster
workflow.

[Run 35171991156](https://github.com/jsell-rh/hypershell-stego/actions/runs/35171991156)
at `4dd7d21` passed cleanup of every Gateway and final managed-cluster deletion
with the corrected fixture lifetime. The supplied PostgreSQL server and
installation data remained. Three automation accounts used the actual Gateway
and passed durable cleanup. Editor, access recovery, dashboard sign-out, and
worker telemetry checks also passed. All 412 repeated and archived generation
hashes matched. Independent cleanup passed at `2026-09-17T02:08:59Z`.

The test failed after 673.62 seconds at the final PostgreSQL telemetry check.
Its operation list omitted `schema`, which the common runtime documents and
emits for managed schemas. The saved logs contain 79 successful and two canceled
schema events. Hypershell `1f6f846` adds this operation to the acceptance check
and tests its logs, traces, and metrics together. Unknown operations remain
invalid. Both focused SQL signal tests passed in 0.015 seconds. No application
or generated runtime code changed. See the [final cleanup record](https://github.com/jsell-rh/hypershell-stego/blob/e8bb965/acceptance/dashboard-final-cleanup-evidence.json).

Final PostgreSQL telemetry, final browser log privacy, the complete public
workflow, and the expanded CNPG gate still require a passing run. A failed run
does not produce the final after-test generation manifest.

## Earlier editor integration

STEGO compiler `8e0fae6` supplies the browser session backend, captured assets,
browser client and telemetry packages, local application transport, deployment
renderer, and owned image pull Secret operations. Its common runtime passed
[all six CI jobs](https://github.com/jsell-rh/stego/actions/runs/35160694463).
Browser clients request `openid` by default. Additional scopes must be explicit.
See [browser authorization scopes](browser-authorization-scopes.md).

Hypershell runtime `399a41b` composes these components with local application
archetypes. It contains no copied common component metadata. Its controller
prepares separate console storage and identity inputs, deploys the upstream
application with the generated backend, and publishes the console address after
readiness checks. The frontend imports common browser client and telemetry
packages. These checks do not prove the complete deployed workflow.

The Gateway console module at `b875ab2` passed
[source, dependency, image, and repeat-generation checks](https://github.com/jsell-rh/hypershell-stego/actions/runs/35161331243).
All 121 archived source files and 116 downloaded module output and dependency
files matched. The published image contains the checked binary. Hypershell
`399a41b` selects this module and records its final dependency input hashes.
The [journal recovery check](https://github.com/jsell-rh/hypershell-stego/actions/runs/35161331242)
passed all 28 required tests with no skips. These results do not replace the
rendered application test.

[Live run 35161493736](https://github.com/jsell-rh/hypershell-stego/actions/runs/35161493736)
passed API Pod replacement, separate database access checks, verified HTTPS,
login, workspace creation, and visible editor text. All 404 repeated-generation
hashes matched. The actual browser trust store matched the operator's CA. Both
Gateways and dashboards were ready without restarts.

The test failed after 267.44 seconds when it clicked Monaco's input textarea.
The captured stylesheet places this textarea behind the visible editor. The
screenshot shows policy text, but keyboard input and full styling remain
unproved. The network record reached its 128-entry limit and omitted the policy
diagnostics. Later recovery, viewer revocation, full telemetry correlation, and
deletion checks did not run. Independent cleanup passed at 23:32:38 UTC.

Hypershell test change `d0b4a71` clicks the visible editor and checks keyboard
focus. It captures bounded policy directive names before page scripts run and
reserves room for those diagnostics. The application content policy is unchanged.
[Live run 35162964198](https://github.com/jsell-rh/hypershell-stego/actions/runs/35162964198)
at `7c08e61` failed after 279.95 seconds. Native editor focus passed. The
screenshot shows `{}` after the test entered an opening brace: editor pairing
made the test input valid JSON. Hypershell `4d812fa` changes the input to
`{invalid` and checks that it appears before checking rejection. Syntax checks
passed; the revised interaction has not run in the cluster.

The early listener also recorded `style-src-elem` and `style-src-attr`
violations. The screenshot shows incomplete editor styling. This is a separate
integration defect; the input correction does not resolve it. A checked common
solution for dynamic styles is still required. No content-policy rule changed.
The document's policy record does not establish worker behavior. Later recovery,
viewer revocation, full telemetry correlation, and deletion remain unproved.
All 404 repeated-generation hashes matched. CI retained the failure artifacts.
Independent cleanup passed at `2026-09-16T23:51:44.244591Z`, with an empty lease.

The separate [full CI run](https://github.com/jsell-rh/hypershell-stego/actions/runs/35161492272)
at runtime revision `399a41b` passed its core, browser, web-console, and service
image jobs. It recorded 302 passing top-level Go tests and four skipped live
tests. CNPG and Sandbox jobs were not selected. These results do not replace
the failed live application check.

## Earlier live checks

The [live run at f182ec5](https://github.com/jsell-rh/hypershell-stego/actions/runs/35153112538)
failed after 258.34 seconds. All 366 repeated-generation hash entries matched.
Both dashboards and generated backends became ready without container restarts.
Verified TLS database isolation, denied Gateway RPC calls, and provider data
recovery after Gateway Pod replacement passed. The next dashboard `/readyz`
request failed before browser navigation. The probe did not retain the transport
error type, so the cause remains unknown.

Independent checks after the failure verified both public certificates against
the declared CA and hostnames. Each served leaf matched its configured
certificate. Those checks ran from the operator workstation; they do not prove
connectivity from the test Pod at failure time. Independent cleanup passed at
`2026-09-16T21:50:19.127246Z`, with no test runtime, fixture resources, allocated
namespaces, or old test volumes, and an empty test lease.

The test browser now uses the declared public CA in its own bounded, temporary
NSS trust store. This supports certificate replacement during namespace recovery.
Actual trust initialization was checked, but rendered login remains unproved.
Hypershell `4134bda` adds fixed transport error categories without changing TLS
verification or request deadlines. Focused probe checks passed in 0.052 seconds.
The detailed source and failure records remain in Hypershell's
[transport record](https://github.com/jsell-rh/hypershell-stego/blob/4134bda/acceptance/dashboard-transport.md).

The [next live run at 906f75c](https://github.com/jsell-rh/hypershell-stego/actions/runs/35155975877)
opened the browser before Gateway Pod replacement. Its verified HTTPS and
protected-document probes passed. The browser reached the identity provider
and submitted credentials, then received STEGO's sign-in fallback at
`/workspaces`. The test failed after 279.79 seconds. Later restart and recovery
checks did not run. Independent cleanup passed at
`2026-09-16T22:21:40.225959Z` with an empty lease.

The [protected login change](browser-login-document.md) addresses the possible
Strict-cookie transition in common STEGO code. The browser did not record its
cookie exclusion reasons, so that cause remains an inference. The candidate
at `883ca13` also rejects unsupported component pins. Hypershell `1a5fa8a`
regenerates all three modules from that compiler and common registry. Its
[module check](https://github.com/jsell-rh/hypershell-stego/actions/runs/35157957548)
passed. All 83 archived source files matched the local module, and the image
binary and published digest matched. The combined compiler checks have passed.
The deployed workflow check is still required to qualify the application.

The [live run at 55d66c2](https://github.com/jsell-rh/hypershell-stego/actions/runs/35159150673)
then completed login through the new callback document. It reached the upstream
Workspaces page, created a workspace with HTTP 201, and read its detail with
HTTP 200. It opened the global policy dialog, but the editor was blank. The
test failed after 275.33 seconds. Later recovery and access assertions did not
run. All 366 repeated-generation hash entries matched. The saved browser
evidence copied from the test Pod matches the CI archive. Cleanup passed at
`2026-09-16T22:59:48.769861Z` with an empty lease.

The dashboard build omitted PatternFly's documented registration of a local
Monaco instance. Its locked loader defaults to a CDN. Hypershell `59707a9` adds
that build input and keeps STEGO's content policy unchanged. Source CI and a
new rendered editor result are required. The exact browser exclusion reason
from the failed run was not recorded. See the [editor record](https://github.com/jsell-rh/hypershell-stego/blob/59707a9/acceptance/dashboard-editor.md).

The application gate must still prove the rendered workspace and policy editor,
correlated authenticated telemetry, viewer and revoked access, session database
recovery, worker restart, and deletion. Terminal behavior remains a separate
requirement. Passing module builds or common runtime tests cannot close those
application requirements. Hypershell `main` remains at the previously qualified
`d6b1fe3` while the development branch completes these checks.

The stages below preserve earlier implementation work and its test limits.
Their former implementation gaps do not describe the current compiler.

## Source contract

The reference image declares source revision
`07f1b13ebd9e1826afe943b831b092be8bf92498`. This is image metadata, not verified
build provenance.

- The dashboard has a Go API and serves its own UI.
- `LISTEN_ADDRESS` can restrict its HTTP listener to `127.0.0.1`.
- Its authentication middleware accepts a server-supplied bearer token. The
  Gateway validates that token. The dashboard uses mutual TLS for Gateway RPCs.
- Browser requests do not supply a STEGO CSRF token.
- The terminal uses a WebSocket connection. Log retrieval uses HTTP polling.

Source:
[server](https://github.com/Gkrumbach07/openshell-dashboard/blob/07f1b13ebd9e1826afe943b831b092be8bf92498/backend/cmd/server/main.go),
[authentication](https://github.com/Gkrumbach07/openshell-dashboard/blob/07f1b13ebd9e1826afe943b831b092be8bf92498/backend/internal/auth/proxy.go),
[terminal](https://github.com/Gkrumbach07/openshell-dashboard/blob/07f1b13ebd9e1826afe943b831b092be8bf92498/backend/internal/api/terminal_handler.go),
[browser client](https://github.com/Gkrumbach07/openshell-dashboard/blob/07f1b13ebd9e1826afe943b831b092be8bf92498/frontend/src/api/client.ts).

## First implementation

The common HTTP client renderer can now emit a separate local application
constructor. The compiler fixes an unprivileged port. The transport connects
only to that port on `127.0.0.1` through TCP. It does not resolve DNS, use an
environment proxy, or follow redirects. The existing request, response, stream,
connection, and time limits apply. The existing exchange code supplies tracing
and cancellation. The ordinary constructor still requires verified HTTPS.

This constructor is internal compiler support. No component declaration enables
it yet. Existing generated applications do not change. Deployment generation
must prove that the upstream listener is local to the same Pod before this
transport is enabled. This does not change the database TLS policy.

The small local check passed on 2026-09-15. It ran three generated runtime tests
with the race detector. These tests checked HTTP request shapes, stream delivery,
blocked destination changes and redirects, ignored proxy settings, HTTPS
requirements for the ordinary constructor, and cancellation on shutdown. This
used a test HTTP server, not the upstream dashboard image.

Result log:
`/home/jsell/.local/state/stego/runs/dashboard-local-transport-20260915/check.log`.

## Browser session integration

The browser backend renderer now has an internal local application mode. It
uses the existing SQL session store, encrypted tokens, OAuth flow, refresh,
logout, and HTTP telemetry. It forwards the server's bearer token and a small
set of HTTP headers. It does not forward browser cookies or identity headers.
It serves declared UI routes and the application's asset paths through the
authenticated proxy.

Application writes require one exact `Origin` header. If `Sec-Fetch-Site` is
present, it must be `same-origin`. Missing Origin fails closed. Logout keeps its
CSRF-token check. The ordinary management console also keeps its CSRF-token
check. This follows the origin-validation approach described in the
[OWASP CSRF guidance](https://cheatsheetseries.owasp.org/cheatsheets/Cross-Site_Request_Forgery_Prevention_Cheat_Sheet.html).

The compiler fixes the application port. Runtime API address overrides fail.
No service YAML setting enables this mode yet. The current HTTP
body limits also remain in effect; full file-transfer behavior is not proved.

Local generation and build checks passed. Two generated checks passed with the
race detector: exact-origin protection and rejection of runtime address
overrides. Four session tests were skipped because local PostgreSQL was not
configured. CI must run those tests with its required PostgreSQL fixture. They
cover login, identity-header removal, restart, writes, upstream denial, refresh,
and logout across backend instances. They use a test application server.

CI has now completed those checks. All four jobs passed for both
[the transport change](https://github.com/jsell-rh/stego/actions/runs/35020649258)
at `e30174aa54da00d0b0835125c8f678a26d67b0b7` and
[the session change](https://github.com/jsell-rh/stego/actions/runs/35021067251)
at `7d05c2d94f71f44cbee94b44fa9b1b125b8d71ab`. The compiler job ran all tests
with the race detector and required PostgreSQL. The session checks were not
skipped in that environment.

The ordinary browser backend's generated files were also compared with the
previous templates. They were byte-identical for the existing test declaration.
Results and comparison sources are in
`/home/jsell/.local/state/stego/runs/dashboard-browser-20260915`.

The upstream `GetWhoAmI` handler asks the Gateway for identity. A separate user
header would permit a display-only fallback after an RPC error. The proxy
therefore forwards the bearer token and omits that header. The terminal UI has
no automatic reconnect; a new connection starts a new shell. The WebSocket
integration must account for this behavior when it ends an expired session.

## WebSocket integration

The internal application renderer now includes a WebSocket proxy. It requires
the browser's exact HTTPS origin and a valid session. It completes the upstream
authentication check before it accepts the browser upgrade. It sends only the
server-held bearer token and compiler-owned origin, plus trace context. The
transport does not follow redirects or negotiate compression or subprotocols.

Socket capacity is separate from ordinary HTTP capacity. A backend permits four
sockets, with two per browser session. Each direction permits 64 MiB and 65,536
messages. A message is limited to 1 MiB. Each connection permits 1,024 received
control frames. Writes have a five-second deadline. A socket ends at the first
of token expiry, session expiry, or five minutes. It does not refresh the token
on an existing upstream connection.

Local logout cancels the session's sockets. A five-second database check detects
remote logout, token replacement, and loss of session storage. Each database
check has a two-second deadline. Backend close and runtime stop cancel socket
work and wait for its release. After a valid upgrade, socket controls replace
the ordinary HTTP read and write deadlines. Normal upstream close messages are
preserved, including terminal exit codes.

The small local checks passed with the race detector. They covered the transport,
admission limits, control and data limits, redirect rejection, and shutdown.
Session checks require CI PostgreSQL. The generated application also has a CI
vulnerability check for its runtime dependencies. These results do not establish
the live upstream dashboard workflow.

Result directory:
`/home/jsell/.local/state/stego/runs/dashboard-socket-20260915`.

The compiler now tracks the complete HTTP handler chain during shutdown. This
includes outer telemetry handlers after socket work ends. Go HTTP shutdown does
not wait for hijacked connections by itself. The small generated lifecycle suite
passed, including delayed and stalled upgraded handlers. A stalled handler
returns the drain deadline error. Both reference examples were regenerated.
CI verification remains required. Keep the application mode unavailable in
service YAML until the lifecycle and deployment checks are complete.

The first WebSocket CI run,
[35022610109](https://github.com/jsell-rh/stego/actions/runs/35022610109), failed
its upgrade-status assertion. The underlying HTTP observer treated status 101
as an informational response and reported 200. The data and exit-code checks,
access denials, and all seven session-end cases passed before that result was
reported. The generated application's vulnerability check did not run after the
test failure.

STEGO now has a shared final-status observer in `otel-tracing` 1.14.2. It preserves
the response interfaces and treats 101 as final. Its small test checks actual
HTTP responses and recorded spans, including early hints, implicit headers,
flushes, and empty reader copies. That test passed. The browser fixture uses
this same observer.

The corrected revision `1fc6ac6e2e3dc3160bd39e1aef805e0c8639f994` passed all
four jobs in [CI run 35023836717](https://github.com/jsell-rh/stego/actions/runs/35023836717).
The compiler suite passed with the race detector and required PostgreSQL. This
includes WebSocket delivery and status observation, denied requests, all seven
session-end cases, blocked-read cancellation, and HTTP handler drain. The
generated local application's vulnerability check also completed successfully.
Both generated examples and SQL provisioning passed. The earlier failures
remain recorded; the corrected run does not erase them.

The complete run record and log are in `ci-final.json` and
`compiler-ci-final.log` in the result directory above. These tests use a test
application server. They do not verify the upstream image, UI, or deployment.

## Upstream build and UI constraints

The source review also covers the pinned dashboard's
[container build](https://github.com/Gkrumbach07/openshell-dashboard/blob/07f1b13ebd9e1826afe943b831b092be8bf92498/deploy/Dockerfile),
[Webpack configuration](https://github.com/Gkrumbach07/openshell-dashboard/blob/07f1b13ebd9e1826afe943b831b092be8bf92498/frontend/webpack.config.js),
and [HTML document](https://github.com/Gkrumbach07/openshell-dashboard/blob/07f1b13ebd9e1826afe943b831b092be8bf92498/frontend/public/index.html).
These sources expose constraints that a test HTTP server cannot prove:

- Production JavaScript uses root paths such as `/main.<hash>.js`. Font and
  image files also use the build output path. The current local proxy's
  `/assets/` rule does not cover these paths. Generate a checked asset contract
  from the selected build before the application mode is enabled.
- Webpack uses `style-loader`, including in production. It inserts style
  elements at runtime. The current `style-src 'self'` policy does not permit
  them. Test the actual UI and use a controlled build or nonce integration.
  Do not add a general inline-script permission to make the UI load.
- The HTML requests Google Fonts. The current policy permits local fonts only.
  Use local assets or verify the fallback; do not add an external dependency
  without an explicit deployment contract.
- Monaco emits worker assets, and the terminal uses xterm. Include the editor
  and terminal in the UI check. A successful landing page is insufficient.
- The reference container build uses mutable base tags and `npm install`.
  Its Go module declares gRPC 1.82.1. The reviewed image metadata does not
  prove its build inputs or that its runtime dependencies have no known
  vulnerabilities. Keep the upstream application, but require a pinned build
  and dependency checks before production qualification.

The source files and their SHA-256 hashes are stored in
`/home/jsell/.local/state/stego/references/openshell-dashboard-07f1b13`.
No browser test or upstream image vulnerability result is claimed here.

## Remaining acceptance work

The dashboard workflow is not complete. The next changes must connect this
runtime to generated deployment and the upstream dashboard image. They must prove:

1. Authentication for UI, API, and terminal requests. Browser-supplied identity
   headers must not reach the dashboard.
2. CSRF protection that works with the upstream UI, with no general bypass.
3. Bounded WebSocket delivery, session expiry and logout, shutdown, and telemetry.
4. Generated Pod deployment with a local dashboard listener, private credentials,
   Gateway mutual TLS, and no Service path that bypasses authentication.
5. A real Gateway dashboard workflow, including a terminal, restart, denied
   requests, and stable regeneration.

Do not claim these results from the transport test.

## Real upstream source checks

The first Hypershell source check, [35108147006](https://github.com/jsell-rh/hypershell-stego/actions/runs/35108147006),
used upstream revision `07f1b13ebd9e1826afe943b831b092be8bf92498` and application
source `3b95976`. The backend built, but its vulnerability check found a reachable
gRPC issue. UI types and build passed. The runtime JavaScript audit found
router vulnerabilities, and STEGO rejected the asset paths. The main JavaScript
file was 4,973,112 bytes, above the 4 MiB asset limit. The output also included
source maps and separate license text files. This is a failed check.

The next source check, [35108622626](https://github.com/jsell-rh/hypershell-stego/actions/runs/35108622626),
used application `9064968`. Its updated Go dependency set built and passed the
vulnerability check. UI types passed, but router tests stopped before execution
because jsdom did not supply `TextEncoder`. The JavaScript audit found a newer
router advisory. The candidate now selects router 7.18.4 and supplies Node's
standard encoder and decoder in the test setup. Those changes still need CI.

The candidate build keeps the upstream API and entry point. It emits assets
under `/assets/`, splits JavaScript, emits imported CSS as files, preserves
license comments, and removes source maps from served output. It uses the
existing font fallback instead of an external font request. These changes do
not prove editor runtime styles, browser security, terminal behavior, or live
deployment. No compiler size limit has been increased from these results.

The original source archive and the modified build tree are recorded separately.
Local evidence is in `dashboard-source-first-result` and
`dashboard-dependency-result` under the persistent Gateway cleanup run directory.
The application evidence records are on `codex/upstream-dashboard-20260916`.
Keep this build work separate from the qualified management-console workflow.

## Captured assets with the private application runtime

The real upstream source build passed in Hypershell run
[35109813205](https://github.com/jsell-rh/hypershell-stego/actions/runs/35109813205).
Its 35 files fit the declared expanded limits. Compiler revision `3e961af`
captured the 1,169,044-byte ZIP twice with identical output. It generated and
built a fresh browser backend, repeated generation, and reported no drift.
Both dependency checks and all eight selected router tests passed. This does
not qualify a rendered dashboard or its deployment.

Browser backend 1.8.0 lets the internal local-application renderer use captured
assets. It uses the same input bounds, HTML checks, and script hashes as the
ordinary browser renderer. With captured assets, only declared files are served;
unknown asset paths do not fall through to the local application.

Captured files require the same active server session as API requests. This
includes HEAD and conditional requests. Responses retain `no-store`. UI routes
can start login. Token refresh and logout checks also apply after backend restart.
The renderer can add the existing browser telemetry settings to captured HTML.
The application still needs to import the generated browser telemetry runtime.

Small local generation checks passed. CI must check the generated runtime with
PostgreSQL, session denial, refresh, logout, WebSockets, and restart. The mode
remains internal. Service YAML cannot enable it until generated deployment
checks enforce the shared Pod, local listener, and private credential mounts.

The captured-session revision `d229f9f89e61512141bed405f7431b10a0020811` passed
all six jobs in [CI 35110353954](https://github.com/jsell-rh/stego/actions/runs/35110353954).
The generated browser package passed with race detection and required PostgreSQL
in 142.188 seconds. This includes the ordinary browser, local proxy, and captured
local application runtimes. The complete log has SHA-256
`8f8b821dcbcdb43a0ad5f75b4df72f794e077adb7e66958ec72e0b1620a12431`.
The private deployment declaration is a separate candidate and is not qualified
by this result.


## Qualified inputs for application integration

The generated deployment Go API passed all six STEGO jobs at `ea30c88` in
[35113183502](https://github.com/jsell-rh/stego/actions/runs/35113183502).
Hypershell used it with the real upstream image and all eight document routes
in [35114093202](https://github.com/jsell-rh/hypershell-stego/actions/runs/35114093202).
Both source and private deployment checks passed. Image pull verification,
secret separation, repeated generation, and both generated builds passed.
These checks did not start a dashboard Pod.

The common confidential browser client at `1db04e6` passed full STEGO CI and
real Keycloak. See [the provider evidence](browser-client-evidence.json).
The next application change must connect these qualified parts to Gateway
recovery and deletion. It must keep the browser client journal separate from
the native client journal, use separate session database credentials, and
publish the console address only after verified readiness. Gateway roles and
placement remain application policy. The full live dashboard gate stays open.

## Application module candidate

Hypershell `36cd949` adds `gateway-console`, a separate application module built
from the checked upstream image and assets. It selects the common browser
backend, telemetry, session storage, health, and deployment components at
compiler `1db04e6`. Its registry preserves the common component metadata and
adds browser telemetry to the browser archetype. It has no handwritten backend.

[Run 35117250309](https://github.com/jsell-rh/hypershell-stego/actions/runs/35117250309)
passed committed regeneration, dependency checks, both Go entry-point builds,
and private deployment checks. All 83 archived files match the committed module.
The application retains its [evidence](https://github.com/jsell-rh/hypershell-stego/blob/33c5a0d/acceptance/dashboard-module-evidence.json).
Later application `78b06d2` adds the module to the frozen Gateway test archives
and generation snapshots. Its 19 small inspection checks passed; the updated
live test harness still requires execution.

This is a candidate application module, not a live Gateway dashboard result.
The Gateway controller does not yet deploy it. The upstream UI still needs to
import the generated telemetry package. Separate session database provisioning,
confidential client recovery, verified address publication, editor and terminal
checks, and complete deletion remain part of the required application gate.

## Protected console credential reads

Compiler commit `25cb9b2` passed all six CI jobs in
[run 35120295453](https://github.com/jsell-rh/stego/actions/runs/35120295453).
The common browser lifecycle now reads credentials only from an open saved
client. It checks the full provider policy before and after the read and rejects
a changed journal. It does not create or repair clients during credential reads.
The real Keycloak check passed policy rejection, explicit repair, and closure.

Hypershell must still authorize the recipient and check its current Gateway
assignment before credential delivery. These checks do not prove a deployed
dashboard or remove the existing requirement for one lifecycle writer.
