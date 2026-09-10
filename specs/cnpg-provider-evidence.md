The CNPG managed-database workflow tests whether the common STEGO runtime can
support a second database provider. The variant previously accepted CNPG catalog
records but its database controller selected only deployment records. Deleted
CNPG rows therefore kept a correct cleanup obligation without a provider that
could discharge it.

Hypershell now selects the provider explicitly. It uses the existing generated
keyed controller, retained reads, conditional observation commits, cleanup
summaries, bounded HTTP and gRPC clients, and Kubernetes ownership checks. CNPG
resource definitions and readiness rules remain in the variant. This change
adds no compiler-specific CNPG type and no second application queue or retry
framework.

The provider requires the Cluster, Database, and DatabaseRole APIs before
external changes. It creates a shared Cluster and checks the running primary
Pod against the current Cluster UID, pinned image, resource limits, and readiness.
Every pass repairs the desired Cluster, including after a ready observation.
Deletion confirms Cluster absence before it deletes the owned namespace. Both
deletes use generated UID and resource-version preconditions. Completion still
requires namespace absence and an authorized conditional API commit.

This provider illustrates a limit in external observation contracts. CNPG 1.30
does not set `observedGeneration` on its Cluster ready condition. A healthy phase
cannot certify that the operator has applied every current setting. The variant
checks concrete Pod properties as well and states the remaining limit. STEGO
must not invent a generation guarantee for an external API that lacks it.
See the [CNPG source](https://github.com/cloudnative-pg/cloudnative-pg/blob/v1.30.0/pkg/resources/status/transactions.go).

The application gate is documented in the variant's
[CNPG workflow](https://github.com/jsell-rh/hypershell-stego/blob/main/acceptance/cnpg-database.md).
It covers REST catalog creation, generated event delivery and gRPC observations,
shared Gateway placement, encrypted SQL, restart, specification repair, and
cleanup after an API restart and a denied Kubernetes deletion. The CI job also
checks generation from the pinned compiler.

The initial profile has one PostgreSQL instance and 1 GiB storage. This evidence
does not establish high availability, production capacity, backup or restore,
upgrade safety, or generation-specific status for all external settings.
The provider-only gate does not establish Gateway execution on CNPG. The later
Gateway work is recorded below. Database placement history, durable conditions,
cross-process fencing, and production Kafka remain open.

The first complete local provider workflow passed with race detection in
73.43 seconds on 2026-09-10. Five stable provider passes took 48.8 ms. This includes
Kubernetes HTTPS requests and excludes provisioning, SQL, API event delivery,
and concurrent load. Earlier failures exposed two incorrect fixture assumptions
and a missing wait for replacement Pod creation. They did not justify changes
to the catalog access or placement rules.

The resulting common addition is `kubernetes-client` 1.1.0. Its generated
`Client.RequireResources` validates an API group/version and up to 32 unique
resource requirements before I/O. It reads one bounded discovery response,
checks exact kind, namespace scope, and required verbs, and rejects missing or
duplicate required resources. It supports core and grouped API paths. It does
not cache results, authorize operations, or claim operator readiness. HTTP
failures keep their safe status metadata. The application now supplies only
the CNPG resource declarations. Independent generated Widget tests cover the
parser without any Hypershell or CNPG type.

The full STEGO suite passed with race detection and required PostgreSQL. The
independent generated Kubernetes tests and `go vet` passed. The discovery tests
also cover token rotation, removal of API support, invalid requirements before
network access, core API paths, and valid resource names with hyphens.

The final variant commit is
[`3fcc8eb85e4beac7046891e1caf690d540924a5e`](https://github.com/jsell-rh/hypershell-stego/commit/3fcc8eb85e4beac7046891e1caf690d540924a5e).
It pins compiler `934bc01a0f199ae64001e9ccb0c9e80fd0ae96a4`. The complete
fresh-cluster CNPG script passed with generated discovery: 83.19 seconds for the
workflow and 84.247 seconds for the acceptance package. Five stable provider
passes took 48.7 ms. The deployment workload and five related regression tests
passed in 102.420 seconds. All these runs used race detection.

[Compiler CI run 34501670691](https://github.com/jsell-rh/stego/actions/runs/34501670691)
passed for that exact compiler commit. The variant also passed provider unit
checks, all contract tests, CLI build-record checks, and `go vet`. Its new CNPG
CI job runs the same fresh-cluster script and requires pinned regeneration.

A later Gateway review exposed a connection-isolation gap in the shared profile.
The old client rule allowed the application role to connect to the existing
`postgres` database. A regression with the real operator failed against variant
`d01bb29` in 121.64 seconds. The revised profile permits encrypted application
connections only when the role and database names match, then rejects other
client connections. The full revised CNPG workflow passed in 85.54 seconds under
race detection. Both the initial SQL write and the read after restart verify
the forbidden connection. This does not prove isolation of all SQL catalog
metadata or complete Gateway execution.

The variant's [Gateway design record](https://github.com/jsell-rh/hypershell-stego/blob/eef1996/acceptance/cnpg-gateway-design.md)
records two further requirements. Shared namespaces need separate durable key
identities for each Gateway. That key preparation had TLS fixture and race
tests, including concurrent writers and lost material. Those tests covered the
key protocol only. At that revision, the provider still rejected CNPG Gateway
execution. The complete workflow is recorded below.

CNPG role ownership also needs care. Standalone DatabaseRole resources do not
periodically repair direct SQL drift. Inline roles compare SQL state when the
Cluster reconciles. Their status does not identify the desired generation and
cannot alone prove that a role was removed. The first live Gateway drift check
left an added SQL privilege in place for 150 seconds while CNPG still reported
the role as reconciled. Thus inline declarations alone do not establish bounded
repair. The Gateway implementation now uses explicit SQL checks, as described
below.

The shared Gateway path now declares one SQL role, database, password, and
source key identity for each canonical Gateway ID. It uses the same generated
controller runtime as the deployment provider. Hypershell supplies CNPG resource
definitions, readiness checks, and deletion dependencies. STEGO supplies common
scheduling, retries, conditional observations, retained recovery, and Kubernetes
operations.

This workflow required `kubernetes-client` 1.2.0. Its generated `PatchOwned`
method applies a change against the exact UID and resource version used to
compute that change. It does not read a newer version and attach it to an old
list. Independent Widget tests cover conflicts and invalid ownership. Hypershell
uses this method to preserve other Gateways' inline roles during concurrent
changes. Compiler commit `56234ff6ff567b8038792c4b2bf552cc32c2332e` passed its full
PostgreSQL race suite, static checks, and
[CI run 34504429532](https://github.com/jsell-rh/stego/actions/runs/34504429532).

The Gateway checks its own SQL login, privileges, memberships, and database
settings through verified TLS on each pass. Confirmed role or password drift
requests CNPG repair. Unknown connection failures do not request reloads.
Cleanup uses a bound, read-only query to prove that the SQL role and database
are absent before deleting passwords and source keys. Initial key creation also
requires SQL absence, so lost Kubernetes records do not cause implicit rekeying
of retained SQL data.

These checks use a six-second operation limit and fixed session settings.
They add a SQL connection to each normal Gateway pass. Production throughput
and recovery bounds remain unmeasured. Absent role declarations are retained,
with a limit of 1,024 managed roles per Cluster. Safe archive, backup and restore,
SQL database-setting repair, and production HA remain open.

The next complete Gateway run passed SQL privilege, password, and membership
repair, then failed cleanup after 440.12 seconds. The Database resource used
`reclaimPolicy`, but CNPG requires `databaseReclaimPolicy`. Kubernetes pruned
the unknown field and retained the SQL database after resource deletion. The
SQL absence check prevented false cleanup completion.

This exposed another common requirement. `kubernetes-client` 1.3.0 now requires
`fieldValidation=Strict` on create, replace, and patch requests. It preserves
other valid query options, rejects malformed queries before I/O, and does not
permit a caller to select weaker validation. Independent generated tests cover
these rules and safe propagation of schema rejection. This relies on an API
server that supports the
[Kubernetes field validation contract](https://kubernetes.io/docs/reference/using-api/api-concepts/#field-validation).
It does not replace the installed schema or reject fields that a schema
explicitly permits. Compiler commit `b3886715adfb6cdad57c099bd33351fe65ca83d5`
contains this change. The generated client and registry race tests passed.
Its full [CI run 34508032383](https://github.com/jsell-rh/stego/actions/runs/34508032383)
also passed, including the required PostgreSQL race suite and vulnerability check.

The complete CNPG Gateway gate passed with race detection on 2026-09-10.
The workflow took 334.73 seconds; the acceptance package took 335.777 seconds.
Application commit [`0cf98a51900e58ac9b3cccdcad525d93dff481ff`](https://github.com/jsell-rh/hypershell-stego/commit/0cf98a51900e58ac9b3cccdcad525d93dff481ff)
pins compiler `b3886715adfb6cdad57c099bd33351fe65ca83d5`. Pinned regeneration
preserved all 83 generated and dependency file hashes. The deployment Gateway
regression passed in 215.319 seconds with that same compiler.

The gate uses the real Gateway, PostgreSQL operator, identity provider, generated
API, event runtime, and controllers. It covers REST placement and access, gRPC
application calls, encrypted stored credentials, Pod and database restart,
namespace replacement, separate Gateway data and keys, denied SQL connections,
privilege and password repair, stable writes, denied cleanup, and late SQL role
recreation after controller restart. One Gateway's deletion preserves the other
Gateway and the shared Cluster.

Two final test corrections were required. A restart assertion now waits for
the new process to complete its first scan before accepting retained state.
Password repair is checked through authentication with the original Secret;
CNPG can perform that repair before another reload request is needed. Abnormal
shutdown and failed authentication still fail the gate. See the
[application test record](https://github.com/jsell-rh/hypershell-stego/blob/0cf98a51900e58ac9b3cccdcad525d93dff481ff/acceptance/cnpg-gateway.md)
for the full evidence and limits. The enterprise goal remains open.
