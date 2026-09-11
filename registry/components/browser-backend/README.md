# Browser backend

Use this component in a separate browser service. The `browser-service`
archetype adds the common database pool, health checks, and telemetry.
The application supplies its browser assets and its domain API.

Declare `api_prefix`, `routes`, and `assets`. Each asset has a `source` path
relative to the service project and a public `path`. Public assets use
`/index.html` or `/assets/`. Routes must include `/`. A route can contain
`{id}`. An optional `roles_claim` selects a dotted identity claim path.
The compiler captures the declared asset files and checks their sizes.

Apply the generated `browser/schema.sql` migration before service startup.
Set these values in the service environment:

- `DATABASE_URL`: the database connection with verified TLS.
- `STEGO_HTTP_TLS_CERT` and `STEGO_HTTP_TLS_KEY`: the service certificate files.
- `STEGO_BROWSER_ORIGIN`: the public HTTPS origin without a path.
- `STEGO_BROWSER_API_URL`: the separate API HTTPS origin without a path.
- `STEGO_BROWSER_API_CA_FILE`: the API trust root file, if required.
- `STEGO_BROWSER_ISSUER`: the OIDC issuer URL.
- `STEGO_BROWSER_ISSUER_CA_FILE`: the issuer trust root file, if required.
- `STEGO_BROWSER_CLIENT_ID`: the confidential OIDC client ID.
- `STEGO_BROWSER_CLIENT_SECRET_FILE`: the protected client secret file.
- `STEGO_BROWSER_SESSION_KEY_FILE`: a protected file with 32 random bytes
  encoded as standard base64.

Use different host names for the console, its API, and its identity provider.
The API and identity provider can share a host with each other. They must not
share the console host, even on another port. Use ASCII host names, including
punycode for international domain names. Cookies do not isolate ports on one
host, as described in [RFC 6265](https://www.rfc-editor.org/rfc/rfc6265#section-8.5).

Register the exact `STEGO_BROWSER_ORIGIN` plus `/auth/callback` as the client
redirect URI. The identity provider must support authorization codes, S256
PKCE, and token revocation. Grant the access token the audience and roles
required by the domain API. ID tokens must identify the browser client.

The service exposes GET `/auth/login`, GET `/auth/callback`, GET
`/auth/session`, and POST `/auth/logout`. The session response includes
`authenticated`, `expires_at`, `roles`, `user`, and `csrf_token` when signed
in. It does not include OAuth tokens. Mutations require the exact Origin and
`X-CSRF-Token` headers. The API prefix forwards the user's server-held access
token to the configured API. It does not accept bearer tokens from the browser.

The component does not supply domain pages, API authorization rules, identity
provider administration, ingress certificates, or production database setup.
See [the runtime contract](../../../specs/browser-backend.md) for limits and
acceptance evidence. A browser user interface and deployment still need their
own application checks.
