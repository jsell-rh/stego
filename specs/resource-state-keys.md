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

## Prepared statement correction

CI run `35101571163`, source `8496dfc`, failed. The new key coverage,
closed-transaction, and schema-guard tests passed with ordinary statements.
Other store tests found that migration 010 sent two timeout commands in one
prepared statement. PostgreSQL rejected that call with SQLSTATE 42601.
The full compiler result was a failure.

Adapter 4.8.1 sends each timeout command and the index command separately in the
same transaction. The key coverage test now runs with prepared statements both
on and off. Compiler `3a6e00e` contains the correction. Full CI must qualify it.
Run `35101860805` used the same failed migration and was canceled after this
cause was confirmed. No application release uses this candidate on main.
