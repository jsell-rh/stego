The JWT verifier exposes the issuer, subject, and standard profile claims only after
signature, issuer, audience, and time checks pass. Profile claims include
`preferred_username`, `email`, `given_name`, and `family_name`.

`Identity.Issuer` and `Identity.UserID` contain the verified issuer and subject.
Applications must use this pair for persistent identity. A username or email can
change or be assigned to another subject. It must not transfer existing access.
See [OpenID Connect claim stability](https://openid.net/specs/openid-connect-core-1_0.html#ClaimStability).

The optional `roles_claim` setting selects one role array through a dotted
claim path. For example, `realm_access.roles` selects a nested role array. An
empty setting selects no role array. A missing claim returns no roles. A
malformed array fails authentication. The verifier does not use another claim
as a fallback. The existing single `role` claim remains separate.

Claim paths are at most 128 bytes and eight levels. Names can contain ASCII
letters, digits, underscores, and hyphens. Role arrays are limited to 128 entries
of at most 256 bytes each. The existing 16 KiB token limit still applies.

`NewVerifierFromEnvironment` loads one verifier for application transports. It
accepts `STEGO_AUTH_ROLES_CLAIM` as a deployment override. `Authenticate` places
the verified identity in a request context. The generated HTTP middleware uses
this same method. A transport must authenticate before it calls domain code.

Generated runtime tests check profile claims, configured role selection, absent
claims, malformed values, limits, and equal HTTP and shared-context results.
The Hypershell acceptance test uses signed tokens to create a Gateway, preserve
owner access after creator-role removal, and deny creation from another role
claim. This test does not replace REST and gRPC endpoint acceptance checks.
