# SSO authentication

Version 2.0.0 adds SSO claim mapping to the shared `jwt-auth` runtime. It supplies
the `auth-provider` port. It does not supply a second signature verifier, HTTP
client, or key refresh task. Generated output uses `jwt/v5`.

The component requires an explicit issuer and audience before startup succeeds.
Set `issuer` and `audience` in component configuration, or set
`STEGO_AUTH_ISSUER` and `STEGO_AUTH_AUDIENCE` at runtime. An issuer must use HTTPS.
The audience must be nonempty and must match the token audience. No issuer or
audience is inferred from the key source or token.

`jwk_cert_url` selects the trusted key endpoint. Its default is
`https://sso.redhat.com/auth/realms/redhat-external/protocol/openid-connect/certs`.
`JWK_CERT_URL` overrides that setting. `jwk_cert_file`, or `JWK_CERT_FILE`, selects
a local key document and takes precedence over the URL. `jwk_ca_file`, or
`JWK_CA_FILE`, adds a private CA to the system trust roots. Initial key loading
must succeed. The component accepts an absent or `true` `AUTH_ENABLED` value;
all other values stop startup. There is no authentication-disable mode.

The common verifier permits RS256 with a trusted RSA key. It checks issuer,
audience, subject, issue time, and required expiry. It rejects unsupported token
headers, duplicate JSON fields, invalid Unicode, invalid key material, and
oversize input. See [the shared key source contract](../../../jwt-key-source.md)
for TLS, caching, refresh, outage, input, and shutdown limits.

After successful verification, SSO maps these claims:

| Payload | Claim order |
| --- | --- |
| Username | `username`, `preferred_username`, `sub` |
| FirstName | `first_name`, `given_name`, first part of `name` |
| LastName | `last_name`, `family_name`, remaining part of `name` |
| Email | `email` |
| ClientID | `clientId` |
| Issuer | verified `iss` |

`IdentityFromContext` uses the mapped username as UserID. It retains verified
expiry and the email, name, client, and issuer attributes. Role remains empty.
SSO claim mapping does not grant resource access. Payload, username, and verified
token context accessors remain available.

`public_paths` supplies exact paths under the service base path. Defaults are
`/healthcheck` and `/metrics`; an explicitly empty list adds neither. The service
OpenAPI path remains public. Subpaths do not inherit public access. Paths must
be canonical absolute paths without query, fragment, or encoded characters.

`NewJWTHandler` returns a handler and an error. Generated main code handles the
error before serving requests and defers `Stop`. `Build` returns the middleware;
it does not fetch keys or create background tasks. Repeat `Stop` calls are safe.
This changes the old builder constructor API. Regenerate consumers together with
their main code. Old authentication-disable settings must be removed.

Generated runtime tests cover accepted and denied tokens, required claims, SSO
claim fallbacks, exact public paths, error privacy, startup failure, and repeated
shutdown. Shared runtime tests cover key rotation, provider failure and recovery,
verified TLS, redirects, input limits, and cancellation. Full compiler and example
CI must also pass for each released revision.
