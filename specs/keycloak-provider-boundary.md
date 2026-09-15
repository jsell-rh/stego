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
2. A human user can have access to several applications. Reconciliation may
   change only roles for the selected, owned client. Other client roles and
   realm roles must remain unchanged. The user is selected by verified issuer
   and subject, not by a display name.

Do not combine these operations into an unrestricted role-replacement method.
Both operations must remove excess access before they add access. They must
check effective roles, including composite roles, before they report success.

The role extraction also needs a real-provider test for user type. The existing
application rejects a nonempty `serviceAccountClientId` in a user response.
Its absence is not proof of a human user: the reviewed
[26.7.3 user representation](https://github.com/keycloak/keycloak/blob/26.7.3/server-spi-private/src/main/java/org/keycloak/models/utils/ModelToRepresentation.java)
and [user profile](https://github.com/keycloak/keycloak/blob/26.7.3/server-spi-private/src/main/java/org/keycloak/userprofile/DefaultUserProfile.java)
do not set that field on this read path. A client-scoped role operation and an
owned-service-account role operation must remain separate. Before adoption,
prove rejection of a real service-account subject from the human grant path.
Do not copy the existing field check as the complete identity proof.

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

This is the start of the extraction. Enablement, role and scope reconciliation,
and protocol mapper reconciliation remain to be implemented.
Hypershell has not adopted this component. No reduction in its handwritten
client or new Hypershell workflow result is claimed yet. The upstream
dashboard workflow also remains open.
