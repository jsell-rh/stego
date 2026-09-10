# Storage cursor reads

PostgreSQL adapter 3.10.0 adds the optional storage contract
CursorReader.ReadCursor. It reads a page in ascending database ID order with
one query. It does not compute a total or use an offset. The normal List method
keeps its count and page contract.

The cursor value is a bound SQL parameter. It is never appended to search text.
The database collation determines both the continuation predicate and row order.
Callers must not compare the cursor with Go string ordering. A result contains
the entity's typed slice, the last returned ID, and a More flag. The query reads
one extra row to set More. The extra row is outside the returned slice and its
capacity. An empty result has an empty NextID.

Limit must be 1 through 1000. AfterID permits up to 256 UTF-8 bytes and no NUL.
The adapter rejects unknown entities, invalid deletion modes, incomplete scopes,
unknown or duplicate selected fields, and invalid implicit field names. Stored
IDs must also be usable as cursors. Invalid requests return ErrCursor; invalid
stored results return ErrCursorResult. Errors do not return a partial page.
Each call has at most ten seconds, or the caller's shorter deadline.
The limit bounds row count, not encoded byte size.

CursorLive is the default root visibility. CursorAll includes live and deleted
roots. CursorDeleted selects only deleted roots. Related rows still must be
live. Scope, implicit filters, row filters, related filters, and search use the
same query path as List. This includes current-observation projection, so a
stale successful observation cannot change recovery selection.

The reader does not grant access. The application must check the caller before
it reads retained state. It can use the capability on the transaction store to
share the transaction's snapshot and authorization boundary. Separate calls can
see concurrent writes. Start a watch before recovery and repeat scans; this is
not a durable snapshot cursor or a guarantee that one pass sees every mutation.

The first application test found one count and two total reads for each Gateway
or database recovery page. With this reader, each page uses one read and no
count. The same test requires denied callers to perform no read. Hypershell
retains canonical ID validation, access rules, protocol mapping, and recovery
state selection. STEGO owns query construction, ordering, limits, continuation,
and storage filtering.

Generated tests cover C and ICU ordering, exact continuation, live and deleted
visibility, selected fields, lifecycle metadata, related access, bound input,
current observations, invalid requests, invalid stored IDs, transaction reads,
and cancellation while a table lock blocks a query. Application checks cover
database deletion replay through TLS gRPC, missed-deletion recovery, restart,
denied requests, and service-account cursor resumption.

The full compiler race suite passed with PostgreSQL required. Static checks
also passed. The first full run found an old registry-version expectation;
the corrected full run passed.

A local benchmark on 2026-09-10 used Go 1.26.8 on Linux amd64 and an Intel Core
Ultra 9 185H. A 10,000-row fixture had current PostgreSQL statistics. Both paths
requested the first 100 records in ID order and selected names plus required
lifecycle metadata. Three samples each made 100 calls. List took 1.134–1.369 ms,
allocated 171235–172762 bytes, and made 2129–2130 allocations per call. ReadCursor
took 0.474–0.506 ms, allocated 177827–179952 bytes, and made 2154–2155 allocations.
The cursor reads one extra row and validates returned IDs. Other tests ran on
the same host. These samples show the cost of an unused count in this fixture;
they do not establish application throughput or production latency.

```sh
STEGO_BENCH_CURSOR=1 go test -count=1 -v ./internal/generator/postgresadapter -run '^TestGeneratedStoreTransactions$'
```

Set STEGO_TEST_POSTGRES_DSN and STEGO_REQUIRE_POSTGRES=1 for this command.

This change does not provide durable retry storage, cross-process fencing,
cursor persistence, storage indexes for every workload, or production capacity.
