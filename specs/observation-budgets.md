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
