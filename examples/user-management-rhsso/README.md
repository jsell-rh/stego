This example runs on the runtime data path and signs its users in through
Red Hat SSO, a configuration of oidc-auth. The runtime verifier finds the key
set of the declared issuer through its discovery document.
Generate the service with the commands in the repository README.

The declaration names the issuer and the audience of `rh-sso-auth`. Set
`STEGO_AUTHN_CA_FILE` when the provider uses a private CA. The tenant of each
request is the `org_id` claim of its token.

Startup also requires the database, SpiceDB and server configuration described
in the [repository README](../../README.md). Do not use real credentials in
source files.

The [SSO contract](../../specs/registry/components/rh-sso-auth/spec.md) defines
claim mapping and exact public paths.
