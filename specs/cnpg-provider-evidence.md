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
Per-Gateway SQL resources and Gateway execution on CNPG remain open. Database
placement history, durable conditions, cross-process fencing, and production
Kafka also remain open. Complete the CNPG Gateway workflow before using this
provider as evidence that the full application supports CNPG.

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
identities for each Gateway. The prepared key path has TLS fixture and race
tests, including concurrent writers and lost material. It is not yet evidence
of a working CNPG Gateway. The provider still rejects that execution path.

CNPG role ownership also needs care. Standalone DatabaseRole resources do not
periodically repair direct SQL drift. Inline roles do, but their status does
not identify the desired generation and cannot alone prove that a role was
removed. The user was asked about this choice. A bounded, read-only SQL catalog
check is a proposed source of cleanup evidence. It is not implemented, and the
full role, database, credential, and cleanup workflow remains required.
