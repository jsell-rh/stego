# Upstream dashboard integration

The user selected the upstream OpenShell dashboard. STEGO must generate its
common authentication, deployment, and lifecycle support. Hypershell must keep
only Gateway configuration and access rules.

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
No service YAML setting enables this mode yet. WebSocket upgrades return 501
until their session and resource controls are implemented. The current HTTP
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
