# Public response field selection

HTTP application component 1.3.0 adds a bounded JSON projector. Applications
declare public field schemas, parse the requested paths, then project a completed
list response after access checks and presentation. STEGO owns path validation,
wildcards, nested object arrays, response checks, and exact JSON values. The
[component guide](../registry/components/http-application/README.md) defines
the limits and error contract.

Projection does not change SQL columns, counts, page order, or access decisions.
This keeps authorization and derived fields independent of client selection.
Absent fields remain absent. Explicit nulls remain null. The list envelope is
preserved. Whole-object selection still removes undeclared child fields.
Atomic fields permit their complete JSON value and do not accept child paths.

The first application check requests `fields=id,name` from the Hypershell Gateway
list. The old handler returned HTTP 400. The baseline failed in 3.42 seconds,
including setup. The check requires filtered pages and counts, invalid-selector
denial even on empty results, complete gRPC reads, committed events, and restart.
Hypershell's reference declares selection on list items. Its reflection filter
has unsafe slice handling; that implementation is not a compatibility target.

Generated race tests use a separate record service. They check nested paths,
parent/child overlap, wildcards, private fields, nulls, empty arrays, invalid
schemas, malformed responses, exact large integers, copied schemas, and
concurrent calls. Limits bound encoded response size and JSON structure. The
application must still bound the Go values before it calls the JSON encoder.

A local benchmark on 2026-09-10 used Go 1.26.8 on Linux amd64 and an Intel Core
Ultra 9 185H. Three 200 ms samples selected `id,name` from a 100-item page. Each
item also contained an integer and a nested profile. A complete projection took
808.693–832.235 microseconds, allocated 364061–364764 bytes, and made 11801–11802
allocations. Compiler tests ran on the same host. This measures encoding,
validation, and projection; it excludes authorization, database queries, and
network transfer. It does not establish production throughput or a latency gain.

```sh
STEGO_BENCH_PROJECTION=1 go test -count=1 -v ./internal/generator/httpapplication -run '^TestGeneratedApplicationEndpoint$'
```

The first Hypershell integration also projected requests without a `fields`
parameter. Three paired local samples each read 100 pages of 20 visible
Gateways from a 200-row fixture with current PostgreSQL statistics. Full replies
took 3.417–3.638 ms and allocated 513687–525174 bytes per request. Selected
`id,name` replies took 3.047–3.132 ms and allocated 433736–436706 bytes. Reply
bodies were 9705–9710 bytes and 1246–1250 bytes, respectively. The full race
suite ran on the same host. These measurements include JWT checks, database
queries, and an HTTP recorder; they exclude network latency.

The full-reply path did unnecessary encoding, validation, and projection work.
Component 1.4.0 adds `ProjectListIfSelected` to preserve the ordinary response
path when the caller supplies no selector. It returns the original response
without encoding it. An explicit selector still uses all projector checks.
Tests verify both branches, invalid selections, and invalid item-field names.
The endpoint retains its normal response-size and encoding checks. Applications
must supply authorized public response types even when projection is absent.
