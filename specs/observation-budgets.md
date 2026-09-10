# Time for observation commits

Controller component 1.7.0 adds RunObservation. It reserves time for a conditional
observation write after provider work. Work receives the earlier of WorkTimeout
and the parent deadline minus CommitTimeout. The commit callback receives the
work error and a separate context derived from the same parent. A work timeout
does not cancel that context. Parent cancellation stops the commit.

Both timeout values must be between one millisecond and one minute. If only the
commit reserve remains, work does not run; the commit callback receives a deadline
error. The caller can then record an unavailable observation. The callback still
must finish within the remaining parent deadline. The helper does not extend the
parent's lifetime or start a background write after shutdown.

Both callbacks run synchronously and must honor their contexts. They are never
detached. The helper closes each child context, including when work panics. It
preserves the panic. It also checks the clock because timer callbacks can run
late. A nil return after a deadline is an error. Work and commit errors remain
available through errors.Is. A successful commit does not turn failed work into
a successful reconciliation.

Applications own status values, cleanup meaning, authorization, and provider
rules. They must use the resource revision read before work and commit events
with the observation. They must not fetch a newer revision to publish an older
result. A write timeout does not prove that the write rolled back. Retry through
a fresh state read and a new observation. RunObservation supplies no retry,
transaction, fencing, or rollback mechanism of its own.

The application test first established a Healthy Gateway, then held its next
provider call until the action context expired. The old controller used the
same expired context for its failure write, so the API kept Healthy. The test
failed in 25.231 seconds. The equivalent database test kept ready and failed in
25.074 seconds. These tests use REST creation, TLS gRPC controller calls, stored
observations, and generated event delivery.

Hypershell reserves two seconds within each existing 20-second action. It uses
the helper for Gateway workload status, database status, identity configuration,
and all three provider cleanup observations. The original revision and access
checks remain required. A provider deadline can now leave time to record failure
or reopen a previous cleanup confirmation. API outages, lost permissions, parent
cancellation, and concurrent revisions can still prevent the write.

Generated race tests cover the reserved deadline, separate contexts, both error
causes, a short remaining parent budget, cancellation, late nil returns, callback
completion, invalid options, independent calls, delayed timers, and panic cleanup.
The complete compiler race suite and static checks passed.

A local benchmark on 2026-09-10 used Go 1.26.8 on Linux amd64 and an Intel Core
Ultra 9 185H. Three 200 ms samples called the helper with immediate successful
callbacks and no parent deadline. Each call took 833.1–935.3 ns, allocated 544
bytes, and made eight allocations. This measures helper overhead only. It does
not measure provider work, API latency, or production recovery capacity.

```sh
STEGO_BENCH_OBSERVATION=1 go test -count=1 -v ./internal/generator/controller
```

Observation timestamps, durable failure conditions, freshness after controller
loss, cross-process fencing, durable retries, and production targets remain open.

The five application deadline checks passed together in 116.907 seconds. They
cover Gateway and database status, plus Gateway workload, database, and identity
cleanup. Each check confirms failure event delivery, stored failure after API
restart, and recovery with a working provider. Cleanup remains hidden from public
reads. Gateway recovery confirms its current generation, and database recovery
advances its revision.

The database cleanup check also exposed an error in the application capability
handshake. A gRPC stream can report an initial error through Recv after Header
returns no metadata. The old check treated this as permanent missing capability.
An in-memory gRPC test reproduced that error for Aborted, Unavailable,
PermissionDenied, and Unauthenticated. The application now preserves these
statuses. A clean end without the required capability remains invalid. This is
protocol validation in Hypershell; STEGO retains reconnect and retry scheduling.

The real Kubernetes database gate passed in 81.938 seconds. It covered TLS,
persistence, foreign namespace denial, offline deletion, late effects, and
replay with C and ICU database ordering. The complete Kubernetes Gateway gate
passed in 231.422 seconds. It covered database and identity setup, access rules,
service accounts, provider persistence, restart, namespace replacement, offline
deletion, and cleanup on a former cluster. Both gates used the generated time
reserve in the application controllers.

The complete PostgreSQL and Keycloak race suite passed with 112 acceptance tests
and a 917.518-second acceptance package run. Static checks and module verification
also passed. The application commits are `e64f446` for the watch handshake and
`354715aca8d77eecf3ecb25bce38810db7c527ba` for observation budgets. Both are on remote
main. Generation from pinned compiler `46b5f4e5327dfd056cafb65fe3a39ff0cde74500`
passed after commit and preserved all 74 generated and dependency file hashes.
The compiler revision also passed CI in run `34481789390`.

These checks used the repository's pinned fixtures on Linux amd64 with Go 1.26.8
and PostgreSQL 18.6. The Kubernetes gates overlapped part of the full suite. The
elapsed times are local evidence, not production capacity or recovery targets.
The remaining observation and reconciliation requirements above still apply.
