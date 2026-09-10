# Finite recovery streams

Controller component 1.6.0 adds `ScanStream`. A finite replay can stop producing
records without closing its transport. A controller must cancel that attempt
and permit a later recovery scan. A total scan deadline would also interrupt
valid scans that wait for queue capacity. Separate open and receive limits
address the stalled request without timing queue admission.

The application supplies stream setup, protocol validation, and a receive
function. STEGO owns timeout cancellation, callback completion, item limits,
and stream closure. The [component guide](../registry/components/controller/README.md)
defines these obligations and their limits. Live watches use their existing
contract; silence on a live watch does not necessarily indicate a failure.

Generated race tests check an empty stream, exact and excess item counts,
invalid options, missing callbacks, source and emitter errors, cancellation,
open and receive timeouts, and queue waits longer than the receive timeout.
A blocked callback must return before the scanner returns. A value returned
after its receive timeout must not reach the emitter.

A local benchmark on 2026-09-09 used Go 1.26.8 on Linux amd64 with an Intel
Core Ultra 9 185H. Three 200 ms samples scanned 1000 integers followed by EOF.
Each complete scan took 180.223–193.957 microseconds, allocated 136392–136394
bytes, and made 2008 allocations. These figures include context and timer
costs. They exclude transport, queue waits, provider work, and retained storage.
The compiler race suite ran on the same host during this measurement. These
results do not establish application throughput or production capacity.

Repeat the generated tests and benchmarks with:

```sh
STEGO_BENCH_CONTROLLER=1 go test -count=1 -v ./internal/generator/controller -run '^TestGeneratedController$'
```

The first application check uses Hypershell database deletion replay. It creates
and deletes a database through REST, drains the events, and restarts the API.
A test adapter permits the first TLS gRPC replay to open and confirm its header,
then blocks its receive function until cancellation. Without a receive limit,
the check failed after 29.32 seconds, including setup. The application must use
the generated scanner and prove that a later replay completes cleanup and
delivers its new event. This failure does not establish the cause of every
possible transport stall.

Durable retries, complete queue saturation handling, cursor storage, and
cross-process fencing remain separate requirements. A callback that ignores
cancellation can still block; the runtime does not detach it or claim a hard
execution limit for arbitrary application code.
