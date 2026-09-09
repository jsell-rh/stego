# Filter query performance

`postgres-adapter` 3.6.1 puts fields from implicit and related filter maps in a
stable order. Equivalent maps with the same value counts use the same SQL shape.
Values remain bound parameters. This reduces prepared-query cache entries and
planning work without changing filters, authorization, counts, or pagination.
It also applies to related filters inside a row-filter expression.

A generated PostgreSQL regression repeats the same three-field filter with
different map insertion orders. It checks the returned row and total on each
request. With 3.6.0, each case created six prepared statements. With stable field
order, each case must use two: one count and one page query. The test covers
implicit and related maps. A legacy API package named `sort` also compiles; the
new standard-library import must not conflict with a peer package alias.

## Measurement conditions

A Hypershell profile found a separate cost in a newly loaded database. Before
PostgreSQL collected statistics, a count over 200 Gateways used a nested-loop
join. It repeated the grant index scan 200 times and removed 14,950 join rows.
The count took about 6.5 ms and touched about 12,400 shared buffers. After
`ANALYZE`, a hash join took about 0.1 ms and touched 11 buffers. Both custom and
generic prepared plans showed the initial problem. The current-observation view
was not its cause.

Performance measurements must state their data size, access distribution,
statistics state, concurrency, and transaction scope. Hypershell's normal list
benchmarks now collect statistics after fixture loading. A separate benchmark
retains the fresh-statistics case. This is a measurement distinction; application
requests do not run `ANALYZE` or override the planner. Production load procedures
must account for statistics collection after a large import or change.

The generation contract adds fields and row allocations. Stable query order does
not remove that cost. Large-table status filters, concurrent capacity, backlog
recovery, and production latency bounds still require measurements.
