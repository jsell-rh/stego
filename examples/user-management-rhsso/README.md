This example uses the SSO claim adapter and the shared JWT verifier.
Generate the service with the commands in the repository README.

Before startup, set `STEGO_AUTH_ISSUER` to the exact HTTPS issuer of the service's
identity provider and `STEGO_AUTH_AUDIENCE` to this service's token audience.
Set `JWK_CERT_URL` to that provider's HTTPS key endpoint. Alternatively, set
`JWK_CERT_FILE` to a trusted local JWKS file. `JWK_CA_FILE` can supply a private
CA certificate. The key source does not establish the issuer or audience.

Startup rejects missing trust settings, invalid keys, and `AUTH_ENABLED=false`.
It also requires the normal database and server configuration described in the
[repository README](../../README.md). Do not use real credentials in source files.

The [SSO contract](../../specs/registry/components/rh-sso-auth/spec.md) defines
claim mapping and exact public paths. The
[shared key source contract](../../specs/jwt-key-source.md) defines key rotation,
provider outage, request limits, and shutdown behavior.
