# Retained resource-state keys

Postgres adapter 4.8.0 adds the optional `ResourceStateKeyReader` storage
contract. It reads bounded pages of IDs and versions for one exact entity and
scope. The query does not select encrypted contents or join domain records.
Cleared state records remain visible. IDs use byte order and a key cursor.

Use this reader when recovery must find saved provider obligations that have no
domain row. The application must authorize the selected scope. The reader does
not grant access or prove that provider cleanup is complete. Stop new state
registrations before a completed scan can prove coverage. A concurrent insert
before a saved cursor requires another scan. Use the generated controller scan
and checkpoint runtime for bounded work and restart recovery.

Migration 010 adds an index on entity, scope, and resource ID. Migration 009 is
unchanged. Index creation has a five-second lock timeout and a 25-second
statement timeout. Store startup requires the index, its exact key order and
collation, and the existing state guards. This change does not add an application
upgrade procedure for an existing guarded schema.

The local generated-code check passed in 5.93 seconds. Invalid requests and a
canceled context were rejected before database access. Database coverage and
closed-transaction tests were skipped locally because no database was supplied.
CI must prove those tests, exact scope and entity isolation, byte ordering,
cleared records, continuation after store reconstruction, and rejection of
missing or altered indexes. Both examples were regenerated with clean compiler
`e9a7930605a245d95949c8d7e1cd0a62e5ad4a74`; a second apply made no changes.

Hypershell still needs a composed recovery test for the saved orphan omitted
from a provider list. This common reader alone does not close that defect.
