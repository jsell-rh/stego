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
The live extension is pending. It uses the real Keycloak login form to check
authorization-code login, PKCE rejection, callback rejection, signed token
claims, code reuse denial, and device authorization policy for two profiles.
It drives bounded HTTP protocol requests; it is not a browser UI test.

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
