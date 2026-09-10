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
