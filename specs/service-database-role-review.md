# Service database role setup

This review covers STEGO source `5f68b279` and the failed Hypershell service
Deployment at `2af3ec4`. It identifies a remaining setup contract. It does not
qualify the corrected application or close C5, C6, or H3.

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

STEGO already has a related installation boundary for browser sessions. Its
[generated schema package](../internal/generator/browserbackend/schema_package.go.tmpl)
separates bootstrap with owner authorization from runtime verification and supplies the
session-table grants. The PostgreSQL adapter supplies marker bootstrap and
verification, but its documented marker grants still require caller SQL.

Complete the corrected service Deployment workflow before a further runtime
change. Then use this failure to define a common installation contract for
STEGO's private schema permissions. The contract must preserve separate owner
and runtime roles. The service process must not grant itself permissions.
It must not change unrelated roles, remove operator grants, grant access to all
future tables, or conceal missing access by disabling verification.

Application declarations must retain their domain access policy. Common
components must describe and check access to their own objects. A proposed
installation interface must cover storage, outbox, and browser-session needs
without depending on Hypershell names. Verify it with an independent service,
denied writes, incomplete setup, repeated setup, and the complete Hypershell
workflow. No interface has been selected or implemented by this review.
