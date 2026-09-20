# Service database role setup

This review covers STEGO source `5f68b279`, the failed Hypershell service
Deployment at `2af3ec4`, and the corrected workflow at `c85c608`. It identifies
a remaining setup contract. The complete corrected workflow passed; the
common installation interface and C5, C6, and H3 remain open.

The generated API checks the schema-generation marker before it serves
requests. The service fixture granted access to application tables and the
outbox but omitted marker read access. Its API Pod repeatedly stopped before
readiness. The test failed after 180 seconds. Cleanup passed. The Pod startup
log was not retained, so the result does not establish that missing access was
the only startup failure. See the
[failure record](https://github.com/jsell-rh/hypershell-stego/blob/c275c4f1f8a5a479bc657a42c07f1c7691e1e70d/acceptance/service-image-deployment-failure-evidence.json).

The common runtime correctly rejects a role that cannot inspect its marker.
The [schema contract](../registry/components/postgres-adapter/schema-generation.md)
already specifies USAGE on `stego_schema` and SELECT on its `generation` table.
The [generated database tests](../internal/generator/postgresadapter/testdata/schema_generation_test.go)
check denied startup without those grants, accepted startup with read access,
and denied marker writes. The permission requirement has documentation and
common test coverage.

The remaining application setup still repeats the SQL object names and grants
for common storage. The process and browser fixtures already included marker
access; the service fixture had drifted. Candidate `c275c4f` uses one fixture
helper for these three paths. It checks that the runtime can read the marker
and cannot write it or create schema objects. That helper corrects test setup.
A common installation interface and a production role policy remain open.

Candidate `c275c4f` passed the full hosted gate and all seven signed image
checks. Its live service test reached the final telemetry check. Both API Pods
and both identity-worker Pods became ready with the expected images and no
container restarts. This proves that the corrected grants passed service
startup in that run. The complete test still failed: correlated API spans and
logs covered only one of the two instances. Cleanup passed.

The service test retained only 64 batches per signal and read them after all
writers stopped. A full queue can block later exports. Candidate `c85c608`
adds a continuous reader with bounded correlation state. It preserves privacy
checks and requires complete evidence from the same four instances. New checks
cover full queues, replacement instances, missing signals, invalid identities,
and privacy failures. The old result did not record queue saturation. The
correction therefore required its own live result. See the
[telemetry failure record](https://github.com/jsell-rh/hypershell-stego/blob/c85c608f079325c07911cf8a44fea5513511c5de/acceptance/service-image-telemetry-failure-evidence.json).


The corrected workflow passed on jshell at `c85c608`. The full hosted gate
retained all 1,122 prior core cases and passed 20 new cases. All seven signed
image targets passed. The live test checked 407 signal batches and required
complete telemetry from both API and both identity-worker instances. All four
Pods became ready with the expected images and no observed restarts. Creation,
atomic grants, access filtering, rollback, events, HTTPS and gRPC, and Pod
replacement passed. The test took 120.06 seconds. Complete cleanup passed.
The earlier failure remains recorded; its missing saturation data still limits
the causal conclusion. See the [complete service result](https://github.com/jsell-rh/hypershell-stego/blob/4b2440708f213b6f6f269600abca6d2d7aa132dc/acceptance/service-image-deployment-live-evidence.json).

STEGO already has a related installation boundary for browser sessions. Its
[generated schema package](../internal/generator/browserbackend/schema_package.go.tmpl)
separates bootstrap with owner authorization from runtime verification and supplies the
session-table grants. The PostgreSQL adapter supplies marker bootstrap and
verification, but its documented marker grants still require caller SQL.

The corrected service Deployment workflow is complete. Next, use the setup
failure to define a common installation contract for STEGO's private schema
permissions. The contract must preserve separate owner and runtime roles.
The service process must not grant itself permissions.
It must not change unrelated roles, remove operator grants, grant access to all
future tables, or conceal missing access by disabling verification.

Application declarations must retain their domain access policy. Common
components must describe and check access to their own objects. A proposed
installation interface must cover storage, outbox, and browser-session needs
without depending on Hypershell names. Verify it with an independent service,
denied writes, incomplete setup, repeated setup, and the complete Hypershell
workflow. At the time of this review, no interface had been selected.

STEGO source `3603e391` now provides the common object declaration, installer,
and runtime check. It passed the complete compiler gate, independent generated
service checks, and signed release checks. See the
[installation contract](database-access-contract.md) and
[release evidence](database-access-evidence.json). Hypershell adoption and its
complete application workflow are still under qualification. These common
checks do not establish the application result.
