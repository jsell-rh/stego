# Current-source authentication and isolation audit (C4)

This audit covers the remaining C4 scope from the enterprise goal: the shared
JWT runtime, browser sessions, grants, and denied cross-tenant operations at
the current accepted sources, plus DNS-aware enforcement and endpoint failure
behavior. It closes the audit portion of C4 at Hypershell `7f0bd9f` and STEGO
`4d0ce01a`. All file references below were read from `origin/main` on
2026-09-23. Focused checks ran in clean detached worktrees at those commits.

## Shared JWT runtime

`out/auth/middleware.go` `Verify` enforces RS256 only, rejects the `crit`,
`jku`, `jwk`, and `x5u` JOSE headers, and requires a JWT typ. `WithIssuer`,
`WithAudience`, `WithExpirationRequired`, `WithIssuedAt`, and
`WithStrictDecoding` are all applied. RSA keys must be 2048 to 8192 bits with
exponent 65537. `validateTrust` rejects non-HTTPS issuer URLs and an empty
audience before any request is served.

`out/auth/key_source.go` retains verified keys for at most 15 minutes and
enforces a 30-second minimum refresh interval. A refresh makes one bounded
request; there is no background refresh loop, and a failed endpoint fails
closed until the next verified refresh. `out/auth/jwks.go` compiles keys by
`kid` and matches the token key ID exactly.

Stego generator tests at this source cover rotation, endpoint failure, input
limits, redirect refusal, shutdown cancellation, and empty-key failure
(`internal/generator/jwtauth/testdata/key_source_test.go`). The full
`internal/generator/jwtauth` suite passed with the race detector in 11.183
seconds at `4d0ce01a`. The earlier `rh-sso-auth` trust defect and its repair
remain recorded in [sso-auth-audit.md](sso-auth-audit.md) with CI run
`34966748920`.

Hypershell verifies acceptance tokens through `VerifyWithJWKS`
(`acceptance/gateway_login_test.go`, `acceptance/browser_gateway_rpc_test.go`).
Negative cases at the current source cover missing, malformed, wrong-issuer,
wrong-audience, and expired bearer tokens for `/users/me`
(`acceptance/current_user_test.go`), and missing, malformed, and
wrong-audience bearers for Gateway RPC (`acceptance/browser_gateway_rpc_test.go`).
`TestCurrentUserThroughGeneratedRuntime` and
`TestGatewayUserLoginFollowsStoredGrants` passed at `7f0bd9f`.

## Browser sessions

`console/out/browser/backend.go` sets `__Host-Http-stego_session` and
`__Host-Http-stego_login` cookies as Secure, HttpOnly, and SameSite Strict or
Lax. API, issuer, and authorization endpoints must be separate HTTPS hosts.
The login record expires after 5 minutes and the active session after 1 hour.

`console/out/browser/store.go` rejects any stored record whose `Expires` value
has passed before the payload is used, so expiry does not depend on the cookie
alone. `console/out/browser/keys.go` `sessionKeys` accepts 1 to 3 distinct
256-bit keys from a bounded JSON document; the first key encrypts new records,
so adding a key at position zero rotates encryption while earlier keys still
decrypt. Token refresh uses the stored refresh token only while the access
token has more than 15 seconds of remaining life; the provider response is
bounded and strictly validated (`console/out/browser/oauth.go`).

## Grants and denied cross-tenant operations

`out/auth/grants.go` implements an immutable exact-match policy: a nil policy
denies everything, there are no wildcards or role fallbacks, and the input is
bounded and validated at construction. `internal/gateways/options.go` parses
`HYPERSHELL_CLEANUP_GRANTS`, `HYPERSHELL_CONTROLLER_WRITE_GRANTS`, and
`HYPERSHELL_PROVIDER_STATE_GRANTS` through `ParseGrantPolicy` and bounds
control-plane subjects to 32 valid unique values. Every protected operation
requires an exact grant for the verified caller:

- `AuthorizeCleanup` gates the cleanup summary and identity state reads
  (`internal/gateways/cleanup_summary.go`, `internal/gateways/identity_state.go`),
  reached from `GetGatewayCleanupSummary` in `internal/grpcapi/cleanup_summary.go`.
- `controller_writes.go` and `account_provider_state.go` apply the same
  control-plane plus exact-grant pattern to controller mutations and provider
  state.

Role bindings are scoped per Gateway: service-account binding counts filter
on `scope = "gateway"` with the gateway, user, and role identifiers
(`internal/serviceaccounts/service.go`), so a grant in one Gateway does not
grant another. Denied operations return `Unauthenticated` or `PermissionDenied`
as shown in `acceptance/browser_gateway_rpc_test.go` (ungranted caller),
`acceptance/gateway_revision_test.go` (client-supplied status rejected), and
`acceptance/browser_installation_access_test.go` (test actor cluster role and
Secret reads denied with HTTP 403). The public Gateway evidence record
(`acceptance/browser_public_gateway_test.go`) states `unauthenticated_denied`,
`forged_token_denied`, `foreign_audience_denied`, and `ungranted_user_denied`
for the accepted live runs.

`go build ./...`, `go vet`, and `go test` for `internal/gateways`,
`internal/namespaceallocation`, and `internal/serviceaccounts` passed at
`7f0bd9f`; the first two packages also passed with the race detector.

## DNS-aware enforcement and endpoint failure behavior

Public Gateway TLS is the supported DNS-aware enforcement point. The Gateway
operator derives the public name `gw-<namespace>.<domain>` and validates the
served certificate against it: `internal/gatewayworkload/public_tls.go`
`ensurePublicTLS` calls `VerifyServerTLSSecret` with the exact DNS name, and
the generated verifier (`internal/generator/kubernetesclient/tls_secret.go.tmpl`
in STEGO) requires an owned, non-terminating `kubernetes.io/tls` Secret whose
leaf certificate chains to the configured roots, matches the DNS name in the
SAN, and has the server-auth key usage. Private keys must be one complete PEM
block; certificate chains are limited to 16 entries. Configuration is
validated before use: a public domain is required for any public TLS setting,
IP-literal and invalid-label domains are rejected, and system or file roots
are loaded at startup. Certificate renewal and rotation are covered by
`acceptance/browser_public_rotation_test.go` and
`acceptance/browser_public_renewal_test.go`.

Endpoint failure behavior fails closed across the audited paths: an
authentication key-source failure denies requests until a verified refresh
succeeds; an invalid or mismatched TLS Secret returns an observation error and
the workload is not given the certificate; the browser session store rejects
expired records; the OAuth token exchange rejects unbounded or malformed
provider responses. The public Gateway workflow run also exercises
unauthenticated denial on the public endpoint itself.

## Audit result

The current-source audit found no open defect in signature, issuer, audience,
expiry, rotation, browser sessions, grants, or denied cross-tenant operations.
All checks that gate these paths fail closed. This closes the audit portion of
C4. The remaining C4 work, if any, is new live denial evidence on the public
Gateway endpoint from the current source; the accepted Role cycle evidence
already covers this at the previous source. The cleanup-latency target and
capacity proof remain tracked under their own requirements.
