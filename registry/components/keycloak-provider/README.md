# Keycloak provider

This component generates a client for one Keycloak server and realm. It uses
the generated HTTP application client for verified TLS, request limits,
cancellation, and telemetry. It has no component settings or startup hook.
The application constructs it with `New(Options)` and closes it when its owning
service stops.

Version 0.3.0 provides typed client lookup, bounded inventory pages, ownership
inspection, disablement, confirmed deletion, client-secret reads, service-account
user lookup, disabled service-account creation and base configuration, and
checked role reconciliation. It does not yet provide enablement, scope
reconciliation, or protocol mapper reconciliation.
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

`EnsureClientRoles` creates missing role definitions in an owned, disabled
client. `ReconcileUserClientRoles` changes direct roles for one owned client
and one verified provider subject. It preserves realm roles, groups, and roles
for other clients. It confirms removal before adding access and checks direct
and effective roles. Excess inherited access causes failure; this operation
does not remove a shared user's group membership.

`ReconcileServiceAccountRoles` requires the saved subject from the client's
service-account endpoint and an owned, disabled, confidential OIDC client. It
controls the user's full role set. It removes group memberships and excess
realm and client roles before adding any specified role. It checks ownership
and disablement before each mutation, confirms removal, and checks the final
direct and effective roles. It never enables the client.

`RolePolicy` (also named `ServiceAccountRolePolicy`) accepts explicit realm roles and client roles. Each
client grant requires its own expected client binding. Desired roles must be
leaf roles; list them explicitly instead of relying on a composite role that
can change its meaning. Unexpected composite and group access must be removed
or cause failure. The role names and grant policy come from the application.

Role policy permits at most 64 desired roles across at most 16 clients. Reads
permit at most 512 direct roles and 64 group memberships. Role names match
`[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}`. Shared-user subjects match
`[A-Za-z0-9][A-Za-z0-9_.:@+-]{0,254}`, including common federated subject syntax.
The full operation retains the 15-second deadline. A timeout can leave a
partial removal; a later reconciliation must inspect current state.

The shared-user operation does not classify a user as human. Keycloak can omit
`serviceAccountClientId` from a user response, including a service-account user.
The application must retain its verified identity and human grant policy.
Full role replacement is available only through the separate service-account
operation, with its client-to-user binding check.

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

`ReconcileClientScopes` detaches all default and optional shared scopes from an
owned, disabled OIDC client with full scope disabled. This includes the built-in
`service_account` scope. It does not change shared scope definitions. It replaces
client role scopes with the exact leaf roles in `RolePolicy`. It confirms removal
before addition, checks direct and effective roles, and checks each target binding.
The complete operation retains the 15-second deadline. It accepts at most 128
assigned scopes and the role bounds above.

`InspectClientScopes` checks the same scope policy without writes. It can inspect
an enabled client. Neither method checks token claims or enables a client. The
caller must configure and verify explicit protocol mappers before enablement.
Removing shared scopes also removes their token mappers. Ownership checks cannot
make several administrator requests atomic. The controller must retain exclusive
reconciliation, and operator permissions must prevent concurrent policy writes.

`ReconcileTokenMappers` installs typed access-token claims after shared scopes
have been removed. `TokenClaimsPolicy` selects client audiences, literal resource
audiences, client role arrays, a realm role array, and optional service-account
client metadata. The subject mapper is always present. The policy cannot replace
standard identity claims, overlap claim paths, or select arbitrary mapper code.
It permits at most 16 audiences and 16 client role claims. Reads permit at most
128 mappers, with bounded configuration maps and the existing response limit.

The provider removes unknown or changed mappers before it adds missing mappers.
It confirms removal and the exact stored configuration. Correct mappers retain
their IDs. Each mutation checks the owner, disablement, absence of shared scopes,
and target client bindings. `InspectTokenMappers` checks without writes and can
inspect an enabled client. Neither method enables a client or proves an issued
token. Application policy must still select the required audiences and roles.

The mapper configuration follows the Keycloak 26.7.3
[audience mapper](https://github.com/keycloak/keycloak/blob/26.7.3/services/src/main/java/org/keycloak/protocol/oidc/mappers/AudienceProtocolMapper.java),
[client role mapper](https://github.com/keycloak/keycloak/blob/26.7.3/services/src/main/java/org/keycloak/protocol/oidc/mappers/UserClientRoleMappingMapper.java),
and [claim helper](https://github.com/keycloak/keycloak/blob/26.7.3/services/src/main/java/org/keycloak/protocol/oidc/mappers/OIDCAttributeMapperHelper.java).

Keycloak's assigned-scope responses contain only IDs and names. An empty response
can also mean that the administrator cannot view scopes. Before it accepts empty
assignments, the provider requires a nonempty realm scope inventory and confirms
a direct scope read. The inventory has a limit of 512 entries. A realm with no
scope definitions cannot supply this permission proof and is rejected. Client
scope management requires the provider's realm scope permissions; client-only
permissions are insufficient. The provider never changes a scope definition.

Keycloak 26.7.3 uses separate permissions for realm-role and client-role scope
changes. Realm-role scope changes need `map-role-client-scope` permission for
those roles or the broader `manage-realm` role. Client-role scope changes can use
`manage-clients`. Consumers that use only client roles do not need realm
management. The real test keeps its normal operator unchanged and uses a separate
disposable credential for the realm-role scope test.

`NativeClientPolicy` supports public native clients with explicit HTTP loopback
callbacks. It requires PKCE S256 for authorization-code login. Device
authorization is disabled unless selected. The policy permits one through 16
redirects. Each redirect must name `localhost` or a canonical loopback IP address,
with an explicit port. A port wildcard is also accepted for `localhost`,
`127.0.0.1`, and `[::1]`. STEGO stores these wildcard declarations without a port.
Keycloak converts the requested port to the default HTTP port and removes that
port from the URI before it checks the registered path. STEGO never sends a
literal wildcard to Keycloak. Fixed-port declarations retain their port. Paths use unescaped ASCII letters,
digits, `.`, `_`, `~`, and `-`, separated by `/`. Queries, fragments, credentials,
traversal, path wildcards, and encoded path aliases are rejected. Prefer loopback
IP literals; `localhost` remains available for existing callback contracts.
This profile does not support private URI schemes or claimed HTTPS callbacks.

`CreateDisabledNativeClient`, `InspectDisabledNativeClient`, and
`ConfigureDisabledNativeClient` use the same checked creation and repair
mechanisms as service accounts. The caller saves a stable provider ID first.
The profile rejects unknown attributes and requires empty shared scopes. Repair
requires ownership and explicit disablement. It does not change role definitions,
scope assignments, or token mappers. Clearing service-account drift can remove
that client's dedicated provider user. No native method enables login.

The application must use an external browser and verify state, issuer, and
nonce. These obligations follow [RFC 8252](https://www.rfc-editor.org/rfc/rfc8252.html).
The provider profile does not implement the native application's login client.

Both base profiles use the realm's authentication flow. They require an explicit
empty `authenticationFlowBindingOverrides` map, disabled front-channel logout
and surrogate authentication, and no root, base, or management URL. A client
cannot retain a weaker flow override while it passes the base-profile check.
The configuration repair clears these settings while the client is disabled.

Keycloak applies some defaults after creation. After a successful creation
response, the provider checks ownership and disablement before it applies the
complete base policy. It then checks the stored result. It does not repeat the
creation request or follow its `Location` header. An uncertain creation response
still requires a later reconciliation.

Authentication flow updates are patches. To clear an old override, the provider
sends its key with an empty value. It checks the empty stored map afterward.
