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

## Remaining acceptance work

The dashboard workflow is not complete. The next changes must connect this
transport to the existing browser session store and OAuth flow. They must prove:

1. Authentication for UI, API, and terminal requests. Browser-supplied identity
   headers must not reach the dashboard.
2. CSRF protection that works with the upstream UI, with no general bypass.
3. Bounded WebSocket delivery, session expiry and logout, shutdown, and telemetry.
4. Generated Pod deployment with a local dashboard listener, private credentials,
   Gateway mutual TLS, and no Service path that bypasses authentication.
5. A real Gateway dashboard workflow, including a terminal, restart, denied
   requests, and stable regeneration.

Do not claim these results from the transport test.
