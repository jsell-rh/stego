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
`/auth/session`, and GET and POST `/auth/logout`. The session response includes
`authenticated`, `expires_at`, `roles`, `user`, and `csrf_token` when signed
in. It does not include OAuth tokens. Mutations require the exact Origin and
`X-CSRF-Token` headers. The API prefix forwards the user's server-held access
token to the configured API. It does not accept bearer tokens from the browser.

The component does not supply domain pages, API authorization rules, identity
provider administration, ingress certificates, or production database setup.
See [the runtime contract](../../../specs/browser-backend.md) for limits and
acceptance evidence. A browser user interface and deployment still need their
own application checks.

GET `/auth/logout` displays a confirmation form. It does not remove the session
or revoke a token. The form requires the session CSRF value and an exact Origin
match. POST without a form still uses `X-CSRF-Token` and returns 204 after local
session removal and token revocation. Failed revocation returns an error; the
local session stays removed.

Set `logout_scope: identity_provider` to send a confirmed browser form to the
identity provider's discovered logout endpoint after local sign-out. The default
is `console`. Provider sign-out requires a separate HTTPS cookie host and an
endpoint without query parameters. Register the exact console origin plus
`/auth/logout` as a permitted post-logout redirect URI. The generated request
uses `client_id` and `post_logout_redirect_uri`. It does not send an ID token
through browser HTML or a URL. The provider must ask for confirmation without
an ID token hint, as specified by
[OpenID Connect RP-Initiated Logout](https://openid.net/specs/openid-connect-rpinitiated-1_0.html#RPLogout).
The console confirmation page permits form navigation to this provider origin.
Other pages retain the same-origin form policy. A return to the console confirms
only local sign-out; it does not assert that the provider completed sign-out.

An invalid or rejected API session returns HTTP 401 with `error: reauth_required`,
`login_url: /auth/login`, and `statusCode: 401`. The browser can restart login
and set a permitted `return_to` route. Storage failure remains HTTP 503. API
permission denial remains HTTP 403 and does not remove a valid session.

For a production browser build, run this command from the service project:

```sh
stego assets --directory build/client --output ui/assets.zip
```

Declare `asset_bundle: ui/assets.zip` instead of `assets`. Keep the bundle
outside generated output.
The command accepts `index.html` and supported files below `assets/`. It rejects
symbolic links, invalid paths, and files that exceed the limits. The archive
has stable file order and metadata. An invalid input cannot replace an existing
bundle.

Limits are 128 files, 4 MiB per expanded file, 16 MiB in total, and 1 MiB for
the captured ZIP. The compiler reads only captured input bytes. It does not
read a build directory during generation. The generated server permits exact
SHA-256 hashes for inline scripts in `index.html`. Other responses do not get
these script permissions. It does not permit `unsafe-inline` or `unsafe-eval`.

HTML and JavaScript inputs are trusted application code. The HTML checks find
unsupported constructs. They are not an HTML sanitizer. The application must
check its dependencies, rendered pages, accessibility, and browser behavior.

When `telemetry_service_name` is set, the compiler requires one explicit HTML
head element. It rejects an existing `stego-runtime-config` metadata element.
The backend inserts this metadata with the active runtime's public signal
settings. Collector addresses and credentials are not included. The metadata
is HTML-escaped data. It does not require an inline script or a change to the
script policy. The response ETag includes the settings.
