The shared storage contract supports literal text search through
`RowFilter.Text`. `TextMatch` matches a substring in any of its selected string
fields. PostgreSQL supplies case folding from the database collation.

The adapter quotes declared field names and passes the search value as a SQL
parameter. Percent, underscore, and escape characters remain literal. Access
filters, resource scopes, and deleted-row rules apply before count and paging.
Text conditions can also occur inside `All` and `Any` groups.

A text match accepts one through eight unique declared string fields. It accepts
one through 4,096 bytes of valid UTF-8 without NUL. The existing filter limits
also apply: eight levels, 64 nodes, and 65,536 total value bytes. Other types and
undeclared fields are rejected. An empty search must omit the text condition.

These limits bound input, not database cost. Substring matching can scan rows.
Applications must select appropriate fields, require access checks, and measure
representative data and concurrent load before making a capacity claim.
Cursor reads are available through the optional generated CursorReader contract.
ReadCursor uses a bound ID, fixed database order, and one bounded query without
a total or offset. It shares List filters and current-observation projection.
The caller must authorize access and select the required deletion visibility.
See [the cursor contract](../../../specs/storage-cursors.md) for limits,
continuation rules, tests, and application evidence.

`CheckpointStore` stores scan progress with an independent version. It rejects
stale saves, retains the version when a scan completes, and requires a store
transaction for each save. The application must authorize each fixed scope.
See [scan checkpoints](../../../specs/scan-checkpoints.md).

Versioned entities with desired generations can declare condition owners and
names. The generated condition writer requires the observed resource revision
and a transaction. PostgreSQL supplies generation and transition time. Current
views hide old-generation status. See [resource conditions](../../../specs/resource-conditions.md).

The generated process uses a private GORM connection with raw logging disabled.
With `otel-tracing`, version 3.14.0 also generates an instrumented PostgreSQL
pool factory. The generated main owns the pool. Request contexts supply the
telemetry runtime; domain code needs no wrapper. See the
[database telemetry contract](../../../specs/database-observability.md).
Process failures report a fixed stage without driver or query text. See the
[process failure policy](../../../specs/process-failure-privacy.md) for coverage
and limits. Database telemetry remains an open requirement.

Version 3.14.1 rejects entity names that conflict with database helpers or
import aliases before generation. A source-based regression checks every
package declaration and import in the generated database helper.

Version 3.15.0 always generates the pool factory, including without telemetry.
It limits open and idle connections and sets finite connection lifetime and
idle time. Deployment settings can change these bounds. Invalid settings stop
pool creation. See the [pool contract](../../../specs/database-pool-bounds.md)
for defaults, limits, ownership, and acceptance requirements.

Version 3.16.0 bounds each pool connection operation to five seconds. The limit
includes host fallbacks and preserves a shorter caller deadline. It does not
limit an established session. Generated startup also uses a five-second ping
before migrations, constructors, tasks, or listeners. See the
[database startup contract](../../../specs/database-startup.md).
