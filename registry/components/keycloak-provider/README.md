# Keycloak provider

This component generates a client for one Keycloak server and realm. It uses
the generated HTTP application client for verified TLS, request limits,
cancellation, and telemetry. It has no component settings or startup hook.
The application constructs it with `New(Options)` and closes it when its owning
service stops.

Version 0.2.0 provides typed client lookup, bounded inventory pages, ownership
inspection, disablement, confirmed deletion, client-secret reads, service-account
user lookup, and disabled service-account creation and base configuration.
It does not yet provide enablement, role reconciliation, scope reconciliation,
or protocol mapper reconciliation.
It is not the complete provider extraction described in
[the acceptance gate](../../../specs/keycloak-provider-boundary.md).

`ClientBinding` contains an immutable provider ID, a public OAuth client ID,
and expected ownership attributes from application state. Do not derive these
expected attributes from the resource being checked. Ownership inspection is
not an atomic remote transaction. The application must retain its existing
controller serialization and provider administration boundaries.

`CreateDisabledServiceAccount` requires a stable provider ID that the application
has saved before the request. It creates a confidential OIDC client and reads
that exact ID to confirm the result. It does not follow the response location.
A conflict returns `ErrConflict`; it does not cause adoption or replacement.
After an uncertain result, use `InspectDisabledServiceAccount` with the saved
binding and policy before another creation attempt.

The service-account profile requires ownership keys under `stego.owner.`.
Applications select the suffix and value, such as `stego.owner.product` or
`stego.owner.pipeline`. These attributes are public metadata, not secrets.
This namespace keeps ownership data separate from Keycloak protocol settings.
General ownership inspection can still read existing bindings with other keys;
profile adoption requires an explicit migration of those ownership attributes.

`ServiceAccountPolicy` requires an explicit token lifetime from 1 to 3,600
seconds. The application can impose a shorter limit. The profile disables
browser, password, refresh-token, device, CIBA, token-exchange, JWT authorization,
and external-token flows. It requires empty redirect and web-origin lists,
no optional scopes, and no default scopes except Keycloak's built-in
`service_account` scope. The provider assigns the client secret.

`ConfigureDisabledServiceAccount` checks ownership and explicit disablement
before it writes base settings. It checks the stored result. It does not change
credentials, roles, scope assignments, or protocol mappers. Scope drift can
cause this check to fail after a base-setting update. The client stays disabled.
The base profile is not proof of effective roles or token claims. Those checks
remain required before enablement.

The client permits 16 concurrent operations. Each operation has a 15-second
deadline; the underlying HTTP requests retain their five-second limit. Inventory
pages have at most 100 entries and offsets below 10,000. The caller must bound
the complete scan and check repeated identities across pages. Keycloak can omit
records after storage errors; a short page does not prove deletion. Client IDs used
in provider URL paths must match `[A-Za-z0-9_-]{1,255}`. Public OAuth client names
can contain UTF-8 text, with a limit of 255 bytes and no control characters.

Administrator tokens are cached only for part of their reported lifetime.
Each request re-reads the private credential file so a changed credential
invalidates the cache. An unauthorized response clears the matching token.
The client does not replay a failed mutation. A later reconciliation must read
current state before it tries again.

Secret values require an explicit `Reveal` call. Ordinary formatting redacts
them, and JSON serialization fails. Error messages do not include provider
response bodies. The component has no method for arbitrary URLs or raw
administrator requests.

`ResolveServiceAccountUser` can create a missing provider user. Keycloak performs
this action through a GET endpoint when service accounts are enabled. The method
therefore requires the expected client binding before it calls that endpoint.

The wire shapes follow the [Keycloak Admin REST API](https://www.keycloak.org/docs-api/25.0.6/rest-api/index.html).
The review also checked the [26.6.0 client resource](https://github.com/keycloak/keycloak/blob/26.6.0/services/src/main/java/org/keycloak/services/resources/admin/ClientResource.java)
and [client list resource](https://github.com/keycloak/keycloak/blob/26.6.0/services/src/main/java/org/keycloak/services/resources/admin/ClientsResource.java).
Live provider qualification remains required before Hypershell adoption.
