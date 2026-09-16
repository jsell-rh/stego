# Keycloak provider

This component generates a client for one Keycloak server and realm. It uses
the generated HTTP application client for verified TLS, request limits,
cancellation, and telemetry. It has no component settings or startup hook.
The application constructs it with `New(Options)` and closes it when its owning
service stops.

Version 0.10.0 provides typed client lifecycle, role, scope, and mapper operations,
checked native and service-account enablement, and service-account token verification. It reuses the
shared JWT verifier from `jwt-auth` or `rh-sso-auth`. The compiler requires that
verifier and the generated HTTP application client. Application adoption remains open. See the
[acceptance gate](../../../specs/keycloak-provider-boundary.md).

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

Version 0.14.0 adds `SearchClients(ctx, fragment, page)`. It reads one bounded
candidate page with the selected client-name fragment. The provider applies a
case-insensitive contains search. Empty or whitespace-only fragments, `%`, `_`,
backslash, and square brackets are rejected before a request. The query has no
fallback to a full scan. URL characters are encoded as query values.

A search result does not authorize a mutation. Keep expected ownership and
immutable provider IDs in application state, then use the checked lifecycle.
The page uses an offset, not a snapshot. Concurrent changes can move records
between pages. A short page does not prove absence. This query does not provide
resumable inventory cleanup; that common-runtime requirement remains open.

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
that client's dedicated provider user. These three base methods never enable login.

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

`NativeAccessPolicy` supplies the native profile, local leaf-role definitions,
exact role scopes, and token claims. It requires at least one explicit audience.
`ReconcileNativeClientAccess` checks an existing owned client, repairs drift
while disabled, and enables login only after the full policy passes. It then
checks the full policy again. A correct enabled client causes no writes.
`InspectNativeClientAccess` checks that same enabled policy without writes.
Create the client with `CreateDisabledNativeClient` after saving its binding.
User grants remain separate; the application decides who can receive each role.

The compound operation uses one capacity permit and the same 15-second deadline
for all normal work. After a failure, cleanup uses that permit for up to five
more seconds and confirms disablement or absence. Caller cancellation does not
prevent cleanup. Provider shutdown does stop it. A failure to confirm cleanup
includes `ErrAccessDisablementUnconfirmed`; retain the saved binding and
reconcile again. No failed enable request is replayed. Ownership is checked
again before cleanup, so a changed binding prevents a write to another owner.

Keycloak has no transaction for these policy changes and enablement. Use
exclusive reconciliation and persist recovery state. These checks establish the
provider configuration; they do not authenticate a native user or prove the
contents of that user's token. The application must still verify issued tokens
and enforce its access policy. The real provider test covers browser-code login;
complete device approval and token exchange remain a separate test requirement.

`VerifyServiceAccountToken` obtains a fresh client-credentials token for an
owned, enabled confidential OIDC service account. Full scope must be disabled.
It verifies the signature with the common JWT verifier and requires the exact
saved subject, client ID, audience set, role arrays, and signed lifetime. It
rejects extra audiences, duplicate audiences, malformed roles, refresh tokens,
ID tokens, and stale issuance. Signing keys come only from the configured
issuer. The issued token and client secret are not returned to the caller.

`ServiceAccountTokenPolicy` supplies the expected subject, audiences, role-claim
paths and values, and lifetime. Empty role sets permit an absent claim or an
empty array, but reject explicit null. The signed lifetime must match exactly;
`expires_in` can be one second shorter for rounding. This method does not enable,
repair, or disable a client. Checked reconciliation must still verify its base
settings, complete roles, scopes, and mappers, and handle failed token proof.

`ServiceAccountAccessPolicy` supplies the base profile, saved dedicated subject,
complete role grants, role scopes, and token mappers. The provider derives the
expected token roles from the intersection of grants and scopes. It requires a
declared audience. `ReconcileServiceAccountAccess` uses the same enablement and
failure-cleanup mechanism as native clients. All repair runs while disabled.
It checks the complete policy before enablement, then checks that policy and a
fresh signed token after enablement. `InspectServiceAccountAccess` performs the
enabled checks without administrative writes. A correct enabled client causes
no administrative writes during reconciliation either.

The access profile accepts only its declared attributes and a valid provider
secret-creation timestamp. Repair preserves that timestamp and removes unwanted
attributes. It confirms the saved dedicated subject before any role mutation.
A missing subject or changed client-user binding requires recovery; it does not
silently select another user. Create the disabled client and save its subject
before this operation. Keep exclusive reconciliation and saved recovery state.
Failure cleanup uses the existing five-second budget and reports unconfirmed
disablement through `ErrAccessDisablementUnconfirmed`.

`DiscoverOwnedClient` supports read-only legacy discovery by exact public client
ID and expected ownership attributes from application state. It checks the full
record after the bounded search. Save the returned provider ID before any
mutation. Do not use discovery instead of a saved binding in ordinary work.

`ClientOwnershipMigration` maps every legacy ownership key to a distinct key
under `stego.owner.`. It cannot change expected values, provider IDs, or public
client IDs. `MigrateClientOwnership` requires a disabled OIDC client before any
write. It rejects conflicting values and unrelated STEGO ownership keys. It
checks scope visibility and requires an observable existing secret for a
confidential client. Only the ownership attributes change; readback must confirm
all other observed fields and unrelated attributes, including the credential.

Call `PrepareClientOwnershipMigration` while the full prior binding is present
and no target key exists. Save its explicit `Checkpoint()` to encrypted storage
before migration. The checkpoint contains a hash of the complete observed record
except the renamed keys. Its hash includes credential state. Formatting and
ordinary JSON serialization cannot expose the checkpoint; storage requires an
explicit `Reveal()`. `RestoreOwnershipMigration` restores the saved plan.

An uncertain write is not replayed. A later call can finish matching partial
key updates while disabled. Each call checks the original saved hash, including
when all keys have changed. Thus a retry cannot hide a credential or unrelated
field change. A completed migration causes no writes. Save the new binding
before access reconciliation or enablement; those actions change the record
and invalidate the migration checkpoint. Run full access reconciliation before use; migration does not prove
the role, scope, mapper, or token policy and never enables the client.

`VerifiedServiceAccountSecret` applies the same token checks and returns the
exact credential used in that grant. Use this operation for an authorized
one-time credential response. A failed check returns no credential. The result
uses the redacted `Secret` type; only the response boundary calls `Reveal()`.
It does not enable or repair the client. Serialize creation, rotation, and
deletion for the client before you use either token operation.

`InspectServiceAccountRoles` checks the saved dedicated subject, exact direct and
effective roles, and absence of group membership. It accepts an enabled or
disabled confidential service-account client. It performs no administrative
writes. This role check does not prove scopes, mappers, or token contents; use
`InspectServiceAccountAccess` for the complete enabled policy and token proof.

Protected credentials, migration plans, and client pointers redact every Go
formatting verb through `fmt.Formatter`. `String` and `GoString` alone do not
cover numeric formatting verbs. Keep the client as a pointer; do not copy it.

Service-account role reads use the dedicated realm-mapping endpoint as well as
the combined mapping response. Keycloak filters the combined response by role-view
permission. A caller with user-management and client-management permissions can
therefore miss direct realm roles in that response. The dedicated realm read is
required before a mutation, even when the desired policy has client roles only.
An unreadable or contradictory realm set stops the operation before repair.
The combined set still has the same role-count and identity bounds.

The provider test includes a client-only policy with `manage-clients`,
`view-clients`, `manage-users`, and `view-users`, without `view-realm` or
`manage-realm`. It also uses distinct provider and public client IDs and legacy
ownership keys. This does not reduce the separate visibility requirements for
client scopes. See Keycloak 26.7.3
[role-mapping reads](https://github.com/keycloak/keycloak/blob/26.7.3/services/src/main/java/org/keycloak/services/resources/admin/RoleMapperResource.java)
and [role-view permissions](https://github.com/keycloak/keycloak/blob/26.7.3/services/src/main/java/org/keycloak/services/resources/admin/fgap/RolePermissions.java).

## Native client recovery lifecycle

When the application also selects `controller`, STEGO generates
`NativeClientLifecycle`. It uses the common `StateJournal`. The application
supplies a trusted public client ID, ownership values, optional legacy key
renames, and a read-only callback for roles, claims, and native login settings.
No Gateway names or roles are part of this common lifecycle.

The lifecycle saves a generated provider ID before creation. Legacy discovery
runs only when no record exists. A legacy binding is saved before disablement;
the protected migration checkpoint is saved before an ownership change; and the
new binding is saved before full policy repair or enablement. A lost save result
stops the call. A later call loads and authenticates the record before it acts.
A bound client that is missing is an error; this lifecycle does not replace its
provider identity through public-name discovery.

Close saves irreversible cleanup intent before deletion. It keeps the stable
binding and any pending migration checkpoint. It never creates or enables a
client. Later cleanup calls check the saved ID again, so they can remove a late
create after an earlier absence check. Closed records must remain in storage.
Reconciliation cannot reopen them. Unknown record fields, versions, phases,
changed ownership, and mismatched migration plans are rejected before effects.

The application must authorize the journal against the observed live resource
or its retained deletion. It must supply stable keys outside the state database.
Stop old writers before first adoption. Reconciliation for a client must remain
exclusive across processes: the storage version check cannot make remote
Keycloak policy updates atomic. The lifecycle does not supply a leader lease or
detect a whole-database rollback.

The focused generated tests passed with the race detector in both variants in
8.531 seconds. They cover all save boundaries, lost acknowledgements, uncertain
provider results, restart, closed migration, and late cleanup. The real-provider
CI test also covers creation and migration through the complete lifecycle. Its
creation and migration checks passed in CI run `35040036218`. The same checks
with the restricted four-role identity passed in CI run `35040645391`. Hypershell now uses this
lifecycle in its production worker; its updated application checks are pending.

Common binding checks now reject reserved `stego.owner.*` keys that the caller
did not declare. This applies to discovery and later bound operations. A partial
migration cannot become an ordinary binding. It needs its saved migration plan.
`ClientBinding.CheckOwnership` provides the same check for a complete provider
read that an application already holds. It does not make a later remote write
atomic. A regression test reproduced acceptance of both an unexpected owner and
a partial migration before this change.


## Abnormal exit during access changes

After the initial ownership check, the common access operation requires a
completed policy inspection before it can skip cleanup. A panic or `Goexit`
also starts the bounded disablement check. Cleanup uses the saved binding and
its own five-second context. Cancellation of the request does not skip cleanup.
The original panic or `Goexit` still propagates. Cleanup cannot run after process
termination, and an unavailable provider can prevent confirmed disablement.
Durable recovery state and later reconciliation remain required.

A regression test left the client enabled after a panic or `Goexit` before this
fix. The focused generated tests passed with the race detector in both variants
in 11.746 seconds. They cover abnormal exits during the initial enabled check,
repair, and the final enabled check, plus normal native and service-account
access operations. They also check that the operation permit is released.


## Service-account recovery lifecycle

With `controller`, STEGO also generates `ServiceAccountClientLifecycle`.
`ClientIdentity` supplies trusted ownership and optional legacy renames.
`NativeClientIdentity` remains an alias for the existing native-client API.
Both lifecycles use the same creation, migration, and closure implementation.
Existing native records retain their format. Service-account records have a
separate kind; neither lifecycle accepts the other kind.

The service-account lifecycle saves the provider client ID before creation and
the ownership checkpoint before migration. It confirms disablement before it
resolves a subject. It saves that subject before access repair or enablement.
The policy can require an application subject through `ExpectedSubject`; a
saved subject cannot change. A failed save stops the call, including a lost
acknowledgement after commit. A new call reloads and authenticates the journal.
The provider subject endpoint can create a user, so it is an effect, even though
Keycloak uses GET for this operation.

The policy's `Disabled` flag selects complete repair without enablement. The
common `ReconcileDisabledServiceAccountAccess` operation requests no client
credential or token. A later enabled reconciliation retains the saved client
and subject and proves the complete enabled policy through a signed token.
Neither lifecycle returns or stores a credential. The application still owns
credential delivery, quotas, expiry, and authorization for each operation.
Closure remains irreversible and retains the binding for late-create cleanup.

The generated native, service-account, and access tests passed with the race
detector in both variants in 29.110 seconds. They cover each save boundary,
lost provider results, subject replacement, restart, wrong record kinds,
disabled repair, and retained cleanup. The real Keycloak test now exercises
creation, migration, lost subject-save acknowledgements, signed token checks,
disabled repair, resume, and late-create cleanup with the restricted four-role
identity. The new real-provider checks passed in CI run `35041366927`, provider job
`104622531905`; the whole real-provider test took 61.44 seconds. Hypershell
service-account adoption is still pending; the common native lifecycle is already in its worker.


## Concurrent lifecycle calls and saved provider IDs

Lifecycle calls on one provider client now share a gate keyed by the public
OAuth client ID. This includes different journal instances and both lifecycle
kinds. Calls for one client cannot overlap; unrelated clients can proceed.
Admission counts active calls and waiters and is limited to 128. Released keys
are removed. Request cancellation and provider closure cancel queued work. A
lifecycle has a two-minute upper bound, further limited by the caller's deadline.
The gate does not serialize direct low-level provider calls or other processes;
the application must still exclude those writers.

`ServiceAccountLifecyclePolicy.ExpectedProviderID` checks an existing
application binding before effects. A changed ID or a new allocation is rejected.
`CloseExisting` records an application's saved ID without public-name discovery
when no journal exists. It uses the declared legacy ownership if present, and
current ownership otherwise. This also records irreversible cleanup intent when
the client is absent, so a later cleanup can remove a late create with that ID.
A different ID in an existing journal is rejected before provider effects.

The focused generated native and account tests passed with the race detector in
both variants in 17.268 seconds. They check shared gates across journal instances,
independent clients, bounded admission, cancellation, provider closure, released
keys, preserved provider IDs, and cleanup of an absent saved client. The updated
real-provider check now uses saved IDs for recovery and cleanup; its result is
pending.
