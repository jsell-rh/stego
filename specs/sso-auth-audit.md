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
