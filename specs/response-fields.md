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

With component 1.4.0, the same paired benchmark took 2.565–2.632 ms for normal
replies, with 268750–271009 allocated bytes and 3395–3398 allocations. Selected
replies took 3.026–3.427 ms, with 434054–435195 bytes and 8392–8395 allocations.
Normal reply bodies were 9692–9708 bytes; selected bodies were 1244–1250 bytes.
The full race suite again ran on the same host. Field selection reduces transfer
size but adds server work. The normal path avoids that work. These samples do
not establish a production latency target or include network transfer.

The final focused application checks passed in 26.747 seconds. They cover field
selection, grant discovery, role discovery, placement, Gateway networks, and
access-filtered search. The selected Gateway workflow took 4.82 seconds,
including setup. The first full suite found two old expectations that rejected
valid field selection; those checks now reject an unknown field. That failed
run is not counted as a pass.

The final full PostgreSQL/Keycloak race suite passed on 2026-09-10. Its
acceptance package took 673.013 seconds. Variant
`71789814755b22c574c24539fdcef846ad126010` is on remote main and pins compiler
`ee311a8853aa3b89b810794a45d6d8f8b0a265e0`. Regeneration after commit passed;
all 72 generated and dependency file hashes stayed unchanged. Static checks
and module verification passed. See the
[application evidence](https://github.com/jsell-rh/hypershell-stego/blob/71789814755b22c574c24539fdcef846ad126010/acceptance/field-selection.md)
for supported resources, transport checks, and benchmark details.

Compiler revision `ee311a8` passed CI in run `34477137007`. The previous variant
revision `87966df` passed all four CI jobs in run `34475851754`. The field-selection
variant requires its own CI run. The separate Kubernetes workload gates were
not repeated locally for this HTTP change. Reconciliation and the full enterprise
goal remain open; response selection does not close those requirements.
