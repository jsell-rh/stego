# Browser authorization scopes

Browser backend 3.0.0 requests only `openid` by default. The backend previously
requested `openid profile email` for every application. That request conflicts
with the common Keycloak browser client, which removes shared realm scopes and
uses explicit client token mappers. The provider tests used `openid`, while the
backend fixture did not check scopes. These separate checks missed the mismatch.

An application can declare additional scopes when its provider client permits
them:

```yaml
overrides:
  browser-backend:
    additional_scopes: [email, profile]
```

This is a component configuration fragment. Keep the existing service fields
and browser routes. Additional scopes are optional. STEGO accepts zero through
seven distinct names and sorts them for deterministic output. Each name must
contain 1 through 128 ASCII letters, digits, periods, underscores, colons,
slashes, or hyphens, and must start with a letter or digit. Whitespace and
non-string values are rejected. Do not include `openid`; it is always present.
`offline_access` is not supported by this session lifecycle.

This change does not grant scopes at the provider or weaken its access policy.
Do not add `email` or `profile` to a client that has no such scopes. Client-owned
token mappers can supply the declared application claims with `openid` alone.

## Compatibility and checks

The major version records the default change. Applications that require the
previous request must explicitly declare `[email, profile]` and configure the
provider client to permit them. Session identity still requires a verified
subject. Email, name, and preferred username remain optional verified claims.

Focused compiler checks passed in 0.024 seconds. Generated runtime tests now
reject unexpected scopes at the provider fixture and inspect the authorization
request for explicit scopes. The real Keycloak test now requires unassigned
`profile email` requests to return `invalid_scope` without an authorization
code. The [real-provider check](browser-scopes-provider-evidence.json) passed in
79.38 seconds on the candidate source. All six jobs in
[common CI](https://github.com/jsell-rh/stego/actions/runs/35152146945) passed,
including the full compiler tests with race detection.
The complete generated Hypershell workflow must be repeated after adoption.

Hypershell live run `35150654630` passed verified dashboard HTTPS, then reached
`/auth/callback` with HTTP 400 before the login form. Its browser page contains
only `Bad Request`. The retained evidence does not contain the provider error
parameter, so the live failure's exact cause is not yet proved. The scope
mismatch above is established by the two generated component contracts. A new
complete application run is required after common CI and regeneration.
