The example CI audit found a separate authentication gap in `rh-sso-auth`.
The dependency update to `jwt/v4` 4.5.2 fixes the reported parser vulnerability.
It does not supply the application's token trust policy.

Source review of [the generator](../internal/generator/rhssoauth/generator.go)
shows that it calls `jwt.Parse` without an expected issuer or audience. The
component configuration has no required issuer or audience setting. The default
`MapClaims.Valid` method in jwt/v4 4.5.2 permits absent time claims, including
expiry. The generated code does not add a required expiry check. The handler
also has an authentication-disable option and a separate key-refresh lifecycle.

This is an open C4 release requirement. A passing vulnerability scan, fill test,
or Hypershell workflow cannot close it. Hypershell selects `jwt-auth`; the SSO
example selects `rh-sso-auth`. Each generated authentication component needs
its own runtime evidence.

The next change must first reproduce acceptance of signed fixture tokens with
the wrong issuer, wrong audience, or missing expiry. Then connect the SSO claim
mapping to the shared verified JWT runtime. Keep common signature checks, token
limits, verified TLS key loading, refresh limits, and shutdown in that runtime.
Require explicit trust settings and reject an authentication-disable setting at
startup. Retain the declared SSO claim mapping and exact public-path behavior.

Acceptance must cover valid and denied requests, required claims, key rotation,
collector and identity-provider failure, bounded token and key input, repeated
shutdown, and regeneration of the SSO example. The source review identifies the
gap; the signed-token regression and corrected runtime results are still required.

The generated signed-token regression reproduced all three defects: wrong issuer,
wrong audience, and missing expiry each returned HTTP 204 instead of 401. The
valid fixture also returned 204. The initial check failed in 0.777 seconds.

The replacement uses the shared JWT verifier and key source. SSO retains its
claim mapping and exact public paths. Startup now requires issuer, audience, and
valid keys; authentication-disable settings fail. The old independent verifier,
key-fetch code, refresh loop, and jwt/v4 dependency are removed. See the
[current component contract](registry/components/rh-sso-auth/spec.md) and
[shared key source contract](jwt-key-source.md).

The expanded SSO runtime check passed under the race detector in 2.111 seconds.
It covers the original defect and additional missing claims, invalid signatures,
unsupported algorithms, duplicate headers, token limits, claim mapping, startup
failure, and repeated shutdown. The shared runtime check passed in 3.079 seconds
before the final empty-runtime and remote-input cases were added. The final
focused checks passed after those additions: shared JWT runtime 2.881 seconds
and SSO runtime 2.157 seconds. Both generated runtimes ran with the race detector.
Full CI and regenerated example results remain required. These checks do not
establish complete observability or performance coverage.

Both example services were regenerated with clean compiler `94f9fa0`. Validation,
dependency resolution, repeated apply, and drift checks passed. Their generated
output now includes the shared key source. The SSO example uses jwt/v5 and the
error-returning constructor. Full CI remains required for these exact files.
