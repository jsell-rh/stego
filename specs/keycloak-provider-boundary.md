# Keycloak provider boundary

Review date: 2026-09-15.

Hypershell still contains reusable Keycloak mechanisms. At application revision
`f9fc9f2da2b3ffb2a8ef1de9e71ec23a0857a9ec`,
`internal/serviceaccountkeycloak/client.go` has 1,158 lines. With `gateway.go`
and `gateway_users.go`, the package has 1,588 implementation lines. Line count
is evidence of the review scope, not an acceptance measure.

The next extraction must separate provider operations from application policy.
It must reduce handwritten mechanisms in an existing, proved workflow. Moving
the same code into a generated package with Gateway constants is insufficient.

## Responsibilities

| Keep in Hypershell | Put in a common STEGO provider |
| --- | --- |
| Gateway and service-account client ID formats | Typed, realm-bound client lookup and lifecycle operations |
| Ownership attribute names and expected application IDs | Validate ownership before mutation; reject foreign clients |
| `gateway:owner` and `gateway:viewer` mapping to OpenShell roles | Resolve and reconcile specified provider roles |
| Gateway audience, role claim, token lifetime, and enabled flow policy | Apply and verify specified client settings and protocol mappers |
| Stored grants, role ceilings, expiry, and deletion history | Bounded provider inventory, checked responses, and partial-failure recovery |
| Gateway state observations and cleanup checkpoints | HTTPS, private credential reads, token cache, cancellation, and telemetry |

The provider must use typed methods with explicit contexts. Its public contract
must not require callers to construct Keycloak URL paths or decode provider
JSON. It must not contain Hypershell IDs, role names, or claim names. Application
policy must supply those values through validated inputs.

The generated HTTP transport already supplies connection and body limits,
verified TLS, cancellation, and telemetry. Reuse it. A provider component must
not introduce a second transport implementation.

## Two different role operations

The current code has two valid but different ownership boundaries:

1. A managed service account has a dedicated provider user. Its full role set
   and client scope are controlled by the application. Reconciliation removes
   unexpected realm roles and roles for other clients before it adds the
   desired roles. The provider must verify the service-account user binding
   before it uses this operation.
2. A shared user can have access to several applications. Reconciliation may
   change only roles for the selected, owned client. Other client roles and
   realm roles must remain unchanged. The user is selected by verified issuer
   and subject, not by a display name. The application decides which verified
   API principals can receive these grants.

Do not combine these operations into an unrestricted role-replacement method.
Both operations must remove excess access before they add access. They must
check effective roles, including composite roles, before they report success.

The role extraction also needs a real-provider test for user type. The existing
application rejects a nonempty `serviceAccountClientId` in a user response.
Its absence is not proof of a human user: the reviewed
[26.7.3 user representation](https://github.com/keycloak/keycloak/blob/26.7.3/server-spi-private/src/main/java/org/keycloak/models/utils/ModelToRepresentation.java)
and [user profile](https://github.com/keycloak/keycloak/blob/26.7.3/server-spi-private/src/main/java/org/keycloak/userprofile/DefaultUserProfile.java)
do not set that field on this read path. A client-scoped role operation and an
owned-service-account role operation must remain separate. A human-only grant
policy needs positive identity evidence before adoption. Do not copy the
existing field check as the complete identity proof or impose a human-only
policy inside the common client-scoped role operation.

## First extraction gate

Move common administrator authentication and typed client management together
with the role, scope, and mapper mechanisms used by the existing service-account
workflow. Keep the desired client policy and Gateway bindings in Hypershell.
Then use the same provider for the existing Gateway user workflow. Do not add
a dashboard-only Keycloak client as a separate implementation.

The extraction must preserve these behaviors:

- A foreign or mismatched client cannot be adopted, changed, or removed.
- New service-account clients start disabled. Failed setup cannot leave a
  usable credential with incomplete restrictions.
- A repair disables access before it changes settings. Repeated reconciliation
  of the correct state does not rewrite provider resources.
- Creation ambiguity, partial deletion, restart, and late provider completion
  remain recoverable through existing application checkpoints.
- Empty, mismatched, duplicate, or unsafe provider identifiers fail closed.
  The provider must check a creation response before it uses its identifier.
- Credential values cannot enter ordinary formatting, JSON, logs, or traces.
- Cancellation applies while a caller waits for token refresh. Provider errors
  do not copy response bodies or credentials into error messages.

Review concurrent mutation behavior as part of the provider contract. A local
ownership check alone does not prove that a later remote write is atomic.
Preserve the existing worker deployment and serialization assumptions until
the replacement has equivalent evidence.

## Required evidence

Keep the application tests that check Gateway binding, ownership before writes,
foreign-client rejection, deletion confirmation, and per-client human roles.
Run the generated provider tests with a second policy that has different ID,
attribute, role, and claim names. This must show that the provider does not
depend on Hypershell policy.

Re-run the existing service-account and Gateway identity workflows against real
Keycloak after adoption. Include token claims, denied requests, human grants,
repair, partial cleanup, restart, and HTTP client telemetry. Preserve the REST,
gRPC, CLI, and regeneration checks that already cover these workflows. Use CI
or one bounded jshell test; do not run the heavy suite on the workstation.

## First provider implementation

STEGO now has a `keycloak-provider` component with typed client reads, bounded
inventory pages, expected ownership checks, disablement, confirmed deletion,
and protected credential reads. Administrator authentication, token caching,
credential-file rotation, cancellation, and remote response parsing are common
provider mechanisms. The component reuses the generated HTTP transport. It has
no public raw-request method and no Hypershell constants.

The small generated checks passed with the race detector, both with and without
the generated telemetry transport. They used two different ownership policies.
They also checked malformed identifiers and responses, mutation confirmation,
token rotation, cancellation, and trace context and privacy. A test fixture
initially blocked during cleanup; the failed result and the corrected results
are preserved in
`/home/jsell/.local/state/stego/runs/keycloak-provider-20260915`.

The initial component at `e1e1b045099ab4210d13c62a2fe969f4118ecdc2` passed all
four jobs in [compiler CI run 35025244647](https://github.com/jsell-rh/stego/actions/runs/35025244647).
The separate real-Keycloak job also passed at
`b6f814165df869797d1418ff1269814f35426924` in
[run 35025647987](https://github.com/jsell-rh/stego/actions/runs/35025647987).
It checked two ownership policies, foreign-client denial, credential reads,
service-account user resolution, disablement, confirmed deletion, and trace
privacy. The test took 28.23 seconds. Container cleanup was verified.

The reconstruction check calls `Client.Close` and `New` within one process.
It does not restart an application process or Keycloak. The test imports its
clients at setup; it does not prove generated client creation or the Hypershell
workflow. The generated source and results are stored under
`live/first-ci` in the result directory above.

A later fix at `8a748301704748965cbd61b7b6b915710fc4863d` requires an explicit
Bearer token type in the administrator grant. Its small checks passed. The real
provider job passed again at `0ebc3cf01376ea783d3557b6315f3befa2b5a41a` in
[run 35026075691](https://github.com/jsell-rh/stego/actions/runs/35026075691).
All five jobs passed. The first live workflow declaration was rejected
before any job started because it used runner context at job scope. That
failure is retained in the result directory. The corrected workflow passes
the workflow linter.

## Disabled service-account creation

Version 0.2.0 adds creation with an application-supplied stable provider ID,
base configuration repair while disabled, and configuration inspection.
The application must save the ID before it calls the provider. A failed response
does not cause an automatic retry, adoption, or rollback. A later reconciliation
can inspect the saved ID and continue or clean up the owned resource.

The common profile controls protocol settings. Application ownership keys use
the `stego.owner.` namespace so metadata cannot override protocol settings.
Application policy still selects IDs, ownership values, roles, claims, and
token lifetimes within the common bound. Existing clients with other ownership
keys need an explicit adoption migration; a file move is not that migration.

Small generated checks passed for creation, conflict rejection, uncertain
responses, ownership denial, explicit disablement, incomplete configuration,
and confirmed repair. The telemetry build passed with the race detector.
The live extension passed at `84b445767b468ee8b8d388f078fac0220d0b5fc7` in
[run 35026787020](https://github.com/jsell-rh/stego/actions/runs/35026787020).
It created two clients with different application ownership policies. It checked
creation, rejection of token requests while disabled, provider-ID and client-name
conflicts, configuration changes, credential preservation, client reconstruction,
and deletion. The test took 41.97 seconds, and container cleanup passed.
All five CI jobs passed for this revision. Sources, generated
output, and results are stored in
`/home/jsell/.local/state/stego/runs/keycloak-provider-creation-20260915`.

## Role mechanisms

Version 0.3.0 adds client role creation and two separate role operations.
`ReconcileUserClientRoles` changes only direct roles for the selected owned
client. It preserves groups, realm roles, and roles for other clients. Excess
inherited roles cause failure. It does not classify the subject as human.
`ReconcileServiceAccountRoles` requires the saved user identity from the owned
client's service-account endpoint and a disabled confidential client. It removes
all group memberships and excess direct roles before it adds specified roles.

Both operations confirm removal before addition and check direct and effective
roles. The application supplies realm and client role names. Desired roles must
be explicit leaf roles. The full operation checks each target client's binding,
as well as the service-account binding. A human subject supplied to the full
operation fails the service-account identity check.

The small generated tests passed with the race detector and telemetry enabled.
They cover ownership, wrong subjects, inherited access, ignored writes, partial
failure recovery, preserved shared-user access, full service-account cleanup,
and repeated reconciliation without writes. The real-Keycloak role checks passed
at `96386e526db236aedf3b5dd9503ec66a11d4d9ff` in
[run 35028368052](https://github.com/jsell-rh/stego/actions/runs/35028368052).
They used two different application role policies. They checked preserved
shared-user access, rejection of excess inherited roles, group and role removal
for owned service accounts, and rejection of the wrong subject. The complete
provider test took 39.96 seconds, and container cleanup passed. All five CI jobs
passed. This does not prove application login or token claims.
Sources, generated output, and results are stored in
`/home/jsell/.local/state/stego/runs/keycloak-provider-roles-20260915`.

## Scope mechanisms

Version 0.4.0 adds exact role scopes and inspection. The application supplies a
`RolePolicy`. The provider detaches all assigned default and optional scopes,
including `service_account`, without changing their shared definitions. It
removes excess role scopes before it adds specified leaf roles. It confirms
removal, direct and effective roles, each client binding, and disablement.

The operation does not configure token claims. Detaching a shared scope also
removes its mappers. Explicit client mappers and issued-token checks are required
before enablement. Small generated tests passed with the race detector, with
and without telemetry. The real Keycloak scope test passed at `6360ce4` in
[run 35030627861](https://github.com/jsell-rh/stego/actions/runs/35030627861). Sources and
results are stored in
`/home/jsell/.local/state/stego/runs/keycloak-provider-scopes-20260915`.
The scope endpoints follow the
[Keycloak Admin REST API](https://www.keycloak.org/docs-api/latest/rest-api/index.html#_scope_mappings).

## Token mapper mechanisms

Version 0.5.0 adds typed token mapper configuration and inspection. The policy
selects audiences, role claim paths, and service-account client metadata. It
cannot select arbitrary mapper code or replace standard identity claims. Shared
scopes must be absent. The provider confirms removal before addition and checks
the exact final configuration. Correct mappers retain their provider IDs.

Small generated tests passed with the race detector, with and without telemetry.
They cover claim validation, inherited scope rejection, binding checks, ignored
writes, partial failure recovery, and stable mapper IDs. The first check failed
because adjacent Go braces were read as a template action. The correction and
both results are retained in
`/home/jsell/.local/state/stego/runs/keycloak-provider-mappers-20260915`.
The live extension passed at `6360ce42bddda4dd48cc8061abb225dcd1d450c0` in
[run 35030627861](https://github.com/jsell-rh/stego/actions/runs/35030627861).
It checked signed token identity, exact audiences and roles, lifetime, and
metadata for two different policies. It also checked stable mapper IDs, exact
configuration repair, shared scope preservation, and scope permission denial.
The complete provider test took 45.75 seconds. Container cleanup passed. All
five CI jobs passed. Generated source and results are retained under `live-ci` in the
mapper result directory.
Production enablement is not part of this change. The test uses a separate,
explicit fixture action to enable its disposable client.

This is the start of the extraction. Native client configuration and verified
enablement remain to be implemented.
Hypershell has not adopted this component. No reduction in its handwritten
client or new Hypershell workflow result is claimed yet. The upstream
dashboard workflow also remains open.

## First live scope result

The first combined scope and mapper gate failed at `261b2ea` in
[run 35029927597](https://github.com/jsell-rh/stego/actions/runs/35029927597).
It stopped before scope mutations because the provider expected a protocol
field that the assigned-scope endpoint does not return. Container cleanup passed.
The failed source and result are retained under `first-ci` in the mapper result
directory. No mapper or issued-token pass is claimed for that run.

The correction accepts the ID and name representation. The review also found
that Keycloak can hide scopes behind an empty successful list. The provider now
requires a nonempty bounded realm scope inventory and a successful direct scope
read before it accepts empty assignments. A realm with no scope definitions
cannot supply this permission proof and is rejected. This follows the pinned
[client resource](https://github.com/keycloak/keycloak/blob/26.7.3/services/src/main/java/org/keycloak/services/resources/admin/ClientResource.java)
and [scope permission checks](https://github.com/keycloak/keycloak/blob/26.7.3/services/src/main/java/org/keycloak/services/resources/admin/fgap/ClientPermissions.java).

The corrected representation passed, including the live test for hidden scope
lists, but the scope update then returned HTTP 403 at `bf198da` in
[run 35030247238](https://github.com/jsell-rh/stego/actions/runs/35030247238).
Cleanup passed. The pinned
[V2 role permission check](https://github.com/keycloak/keycloak/blob/26.7.3/services/src/main/java/org/keycloak/services/resources/admin/fgap/RolePermissionsV2.java)
requires separate permission for realm-role scope changes. The test now proves
that the normal operator is denied that change but can set client-role scopes.
A separate disposable fixture credential supplies realm management for the
realm-role scope test. No production credential or cluster permission changed.

## Native client profile

Version 0.6.0 adds a public native profile with PKCE S256, explicit loopback
callbacks, and optional device authorization. The application supplies callback
URIs, the device-flow choice, ownership, and token lifetime. The provider checks
canonical addresses and paths, rejects ambiguous or external redirects, and
requires a disabled client for creation and repair. Native configuration checks
the complete attribute set. No method enables login.

Native and service-account profiles now share their base creation, readback,
and repair mechanisms. Small generated tests passed with the race detector,
with and without telemetry. They cover drift, missing fields, ownership,
unsafe callbacks, conflicts, ignored writes, and repeated reconciliation.
Sources and results are stored in
`/home/jsell/.local/state/stego/runs/keycloak-provider-native-20260915`.
The live extension passed at `fae5f39f680e1c33ffa399b46a5ae9521d255bd8` in
[run 35032789086](https://github.com/jsell-rh/stego/actions/runs/35032789086).
It uses the real Keycloak login form to check authorization-code login, PKCE
rejection, callback rejection, signed token claims, code reuse denial, and
device authorization policy for two profiles. It also verifies attribute repair
and rejects callback path, query, and fragment changes. The runtime took 45.19
seconds. Container cleanup passed. All five CI jobs passed. Sources and
results are retained under `default-port-ci` in the native result directory.

This test drives bounded HTTP protocol requests; it is not a browser UI test.
The device checks cover authorization requests, required PKCE, and the disabled
policy. They do not cover user approval or device-token exchange. Production
enablement, ownership migration, and Hypershell adoption remain open.

The first native gate at `0704838` failed before enablement. A diagnostic run at
`bc7c635` identified `backchannel.logout.revoke.offline.tokens` as the differing
setting. Both runs completed container cleanup. The pinned
[OIDC creation factory](https://github.com/keycloak/keycloak/blob/26.7.3/services/src/main/java/org/keycloak/protocol/oidc/OIDCLoginProtocolFactory.java)
resets this setting when there is no back-channel URL. Creation now confirms
ownership and disablement before it applies and verifies the complete policy.
The same review confirmed that flow-override updates require explicit removal
entries; an empty map does not clear existing bindings.

The corrected native profile passed its creation and repair checks at `5b0b235`.
The live driver then stopped because it used the administrator transport for
OAuth login. That transport correctly rejects redirects. The test now uses its
own bounded TLS client to inspect OAuth redirects without following them. The
production transport remains unchanged. The failed result and verified cleanup
are stored under `creation-fix-ci` in the native result directory.

The next native run at `f5d35b9` passed authorization-code login, exact signed
claims, wrong-verifier rejection, and code reuse denial for the first profile.
It then rejected a device-authorization request without PKCE parameters. The
test now supplies S256 parameters and retains the missing-PKCE denial check.
This preserves the required PKCE policy. Cleanup passed, and the partial result
is retained under `browser-driver-ci`. The later passing result is listed above.

The native profile converts port wildcards to Keycloak's loopback registration
with no port. The pinned provider permits any callback port for `localhost`,
`127.0.0.1`, and `[::1]` with this registration, while it matches the path
exactly. A literal trailing wildcard also permits other paths. The provider
must therefore not receive a literal wildcard. The live gate now checks wrong
paths, query strings, and fragments. See the pinned
[redirect check](https://github.com/keycloak/keycloak/blob/26.7.3/services/src/main/java/org/keycloak/protocol/oidc/utils/RedirectUtils.java).

Native attribute repair now sends explicit empty entries for unwanted settings.
Keycloak patches this map and retains omitted keys. The provider confirms the
complete stored set after repair. An ignored removal fails the check. The live
test adds an unwanted attribute before repair to check this provider behavior.

The native fixture keeps `webOrigins: []` explicit when it enables a client.
An omitted value can make Keycloak derive browser origins from the callback
URIs. Production enablement must preserve this empty set and confirm the full
policy after the state change. A payload that contains only `enabled` is not
sufficient for this native profile.

The first exact-path run at `a26d54b` rejected valid login callbacks. It stored
`:80` literally. Keycloak's URI builder removes the default port during its
loopback check, so the saved URI must omit the port. This run failed closed and
completed container cleanup. The corrected conversion now omits the port.
The positive login test remains required; negative callback tests alone cannot
prove a usable registration.

A small test also found a shutdown error race. The HTTP client could stop before
the operation context received cancellation from the provider lifetime. The
provider now checks its lifetime before it returns a transport error. Generated
tests passed with the race detector, with and without telemetry, after this fix.

## Checked native enablement

Version 0.7.0 adds `NativeAccessPolicy`, `ReconcileNativeClientAccess`, and
`InspectNativeClientAccess`. The compound operation combines the existing base,
role, scope, and mapper checks. It repairs only while disabled, verifies all
policy before and after enablement, and does not write a correct enabled client.
The caller supplies application values and saves the ownership binding before
creation. User grants remain separate application decisions.

A failed operation confirms disablement or absence with an independent,
five-second cleanup budget. It reports `ErrAccessDisablementUnconfirmed` if that
check fails. Cleanup retains the operation permit and stops on provider shutdown.
Exclusive reconciliation and saved recovery state remain necessary because
Keycloak does not make the policy changes and enablement atomic.

Small generated tests cover capacity use, unchanged state, drift with shared
scopes, ignored writes, uncertain enable responses, failed post-enable checks,
changed ownership, caller cancellation, and failed cleanup. The real native gate
now uses the compound operation before login and after injected policy drift.
The real test passed at `f27377f7c09eeffd5e8912f0754309a67c63baf3` in
[run 35033171244](https://github.com/jsell-rh/stego/actions/runs/35033171244).
Both native policies used the common operation for initial enablement and repair
of shared scopes, callback settings, full-scope access, and unwanted attributes.
The subsequent login tests verified the signed claims and denial cases. The
runtime took 47.01 seconds; container cleanup passed. All five CI jobs passed. Records and frozen source are in
`/home/jsell/.local/state/stego/runs/keycloak-provider-native-access-20260915`.

Small generated tests passed in 3.992 seconds without telemetry and 10.395
seconds with both variants. Both use the race detector. Later sections cover
service-account token verification and checked enablement. Ownership migration
and application adoption remain open. Hypershell still has its handwritten client; this provider
result does not claim an application source reduction.

## Common service-account token proof

Version 0.8.0 adds `ServiceAccountTokenPolicy` and `VerifyServiceAccountToken`.
The provider uses the generated JWT verifier, checks a fresh signed token, and
compares the exact saved identity, audiences, role arrays, and lifetime. Signing
keys come from its configured issuer. It does not return tokens or credentials.
It confirms client ownership before the credential request, before token
issuance, and after proof. It does not call the service-account-user endpoint,
which can create a missing user.

The token check requires an enabled confidential OIDC service account with full
scope disabled. It remains separate from enablement and does not replace full
configuration, role, scope, or mapper checks. Small tests cover altered signed
claims, malformed claims, signature failure, extra credentials, wrong client
state, and ownership changes. The live mapper gate now uses this common proof
for both application policies. It passed at
`c85fd8c9f6916a43120d050fcb121aa0dbe4ff49` in
[run 35033615735](https://github.com/jsell-rh/stego/actions/runs/35033615735).
Both policies passed the common proof; wrong subjects and audiences were denied.
The runtime took 47.52 seconds, and container cleanup passed. All five CI jobs
passed. Small generated tests passed
with and without telemetry in 11.465 seconds. Compiler preflight and namespace
checks also passed. Frozen source and results are retained in
`/home/jsell/.local/state/stego/runs/keycloak-provider-token-proof-20260915`.

The token check first requires the saved subject to exist and be enabled. It
rejects ID and refresh tokens in the client-credentials response. The next
section covers checked enablement. Hypershell adoption remains open; its
compiler pin and handwritten provider have not changed.

## Checked service-account enablement

Version 0.9.0 adds `ServiceAccountAccessPolicy`, `ReconcileServiceAccountAccess`,
and `InspectServiceAccountAccess`. Both access profiles now use the same
checked-enable and failure-cleanup sequence. Service-account repair requires
the saved dedicated subject, removes excess direct and group access, sets exact
scopes and mappers, and verifies a fresh signed token after enablement. Expected
token roles come from the intersection of declared grants and scopes.

The service-account access profile rejects unknown attributes. It permits and
preserves the provider's valid secret-creation timestamp. A correct enabled
client causes no administrative writes. The caller still must save the binding
and subject before reconciliation and retain exclusive control of writes.
Small tests cover those boundaries, scoped token roles, wrong token subjects,
failed role removal, changed subject bindings, and failed cleanup. The real
Keycloak gate now uses the common sequence for both service-account policies,
including group and configuration drift. It passed at
`94ffb943b1afee9f7ed6f0af4f15ab47a16966b2` in
[run 35034193690](https://github.com/jsell-rh/stego/actions/runs/35034193690).
Both policies passed setup, repair, repeated reconciliation, and fresh signed
token proof through the common operation. The runtime took 55.21 seconds, and
container cleanup passed. Full compiler CI was still running when this record
was written. Small tests passed in 5.363 seconds without telemetry and 13.442
seconds with both variants. Both use the race detector. Frozen source and
results are in
`/home/jsell/.local/state/stego/runs/keycloak-provider-service-access-20260915`.
Hypershell adoption and ownership migration remain open.

## Ownership migration

Version 0.10.0 adds read-only `DiscoverOwnedClient` and checked
`MigrateClientOwnership`. Application state supplies the exact public ID and
legacy ownership values. The caller saves the provider ID before mutation.
The migration only renames ownership keys; it cannot change their values or
client IDs. Writes require explicit disablement. Conflicting ownership fails
before a write. Preparation returns a protected checkpoint of the original
record. Save it to encrypted storage before migration. Matching partial updates
can recover from this saved plan. Every retry checks the original record hash,
so new ownership keys alone cannot hide a credential or unrelated field change.
Save the new binding before access reconciliation or enablement. Readback must preserve unrelated attributes and the complete observed client
record, including an existing confidential credential.

Small tests cover discovery, conflicts, partial writes, ignored removals,
credential changes across retries, exact large numbers, protected checkpoint
serialization, preserved unknown fields, and repeated migration. Both generated
variants passed with the race detector in 13.588 seconds. The live
gate now migrates both native and service-account clients before their login
or signed-token checks. The real provider gate passed at `0c4451e` in
[run 35035012028](https://github.com/jsell-rh/stego/actions/runs/35035012028).
It took 56.85 seconds. Container cleanup and all five CI jobs passed.
Frozen source and results are in
`/home/jsell/.local/state/stego/runs/keycloak-provider-migration-20260915`.
The Hypershell adapter must still adopt these methods and persist legacy
bindings before mutation. No application source reduction is claimed yet.

## Verified credential response

Version 0.10.1 adds `VerifiedServiceAccountSecret`. It returns the exact secret
used for a successful signed token check. The method shares the existing token
verification implementation and returns an empty secret on failure, including
an ownership change after the grant. The caller must serialize client creation,
rotation, and deletion. This supports a one-time application response without a
second credential read or duplicate token parsing in the application.

The generated tests passed with and without telemetry and with the race detector
in 13.668 seconds. The live gate uses this method before its separate signed-token
checks. The real Keycloak gate passed at `cae56de1` in
[run 35035109569](https://github.com/jsell-rh/stego/actions/runs/35035109569).
It took 54.09 seconds. Container cleanup and all five CI jobs passed.
Hypershell adoption is in progress.

Hypershell commit `d031143` adopts `VerifiedServiceAccountSecret` in the real
one-time credential response. Its handwritten client fell from 1,158 to 1,042
lines. Domain bindings, audiences, roles, claim paths, and lifetime remain in
Hypershell. Generated code owns credential reads and token proof. Small adapter
tests and generation drift checks passed; full application CI is pending.
Legacy adoption state and the remaining client administration still need work.

Hypershell commit `6c47b0e` also adopts common exact lookup, full client reads,
and bounded page reads. Domain ownership checks and selection remain in the
application. The three handwritten adapter files now total 1,442 lines. Small
read, ownership, role, and cleanup tests passed with the race detector. The
initial application gate exposed a dirty compiler build record; commit
`881afa6` regenerates from the exact clean pin and matches the clean CI hashes.
Complete application qualification remains open at head `fff749b`.

The application browser acceptance, console, and image checks passed in
[run 35035649199](https://github.com/jsell-rh/hypershell-stego/actions/runs/35035649199)
at `fff749b`. Core acceptance and the complete CNPG workflow are still running.
The separate external browser gate rejected a fixture network profile mismatch
before it created a Job. Hypershell `348f630` uses the operator's existing network
profile with external SQL credentials still selected, and checks that profile
before taking the shared test Lease. The correction needs live qualification.

## Gateway grant policy and provider adoption

The application policy permits people and registered API automation to receive
explicit Gateway owner or viewer grants. The stored issuer and subject select
the identity. An interactive login is not required. An absent Keycloak
`serviceAccountClientId` field is not proof of a human identity.

Hypershell commit `2c8d2c2` uses generated `ReconcileUserClientRoles` for Gateway
grants.
Its adapter supplies the trusted Gateway binding and maps its two domain roles
to OpenShell roles. The common provider owns role lookup, bounded requests,
removal before addition, inherited-access checks, and final role confirmation.
The adapter fell from 137 to 51 lines; the three main adapter files total 1,356
lines. This change does not add Hypershell policy to the provider.

The small adapter suite passed with the race detector in 1.326 seconds. Its 19
Gateway grant cases cover both identity types and failed access changes. The
real application login test now adds API automation registration, REST grants,
provider role checks, denied requests, and removal across restart. That extension
still needs CI qualification. Remaining adoption includes client lifecycle,
scopes, mappers, durable legacy bindings, and checked enablement.

## Service-account role inspection

Version 0.10.2 adds `InspectServiceAccountRoles`. It uses the role inspector from
the complete service-account access check. It verifies the saved dedicated
subject before and after inspection, exact direct and effective roles, and no
group membership. It accepts an enabled or disabled confidential service-account
client. The existing role reconciler still requires explicit disablement.

This method lets an application replace a direct-role-only check without adding
its own group or inherited-role mechanism. It does not authorize enablement or
replace scope, mapper, and signed-token checks. Small tests cover both client
states, group drift, missing and excess roles, inherited access, wrong subjects,
disabled users, missing state flags, and changed ownership. The generated suites
passed with and without telemetry and with the race detector in 14.068 seconds.
The real provider gate also checks group drift and repair; that run is pending.
