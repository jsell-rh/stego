The generated telemetry runtime observes the service database pool. The compiler
passes its existing `*sql.DB` through the typed `OptionalSQLDatabase` resource.
For GORM, this is the underlying SQL pool. A process without a consumed database
dependency receives nil. Telemetry cannot cause a new database connection or
make a database mandatory. Consumers need no pool observer or collection loop.

The runtime registers one callback with its private OpenTelemetry meter. Each
collection reads one `database/sql.DBStats` snapshot. It does not query the
server or open connections. The existing export interval, transport, queue,
cardinality, and shutdown limits apply. Shutdown performs the final collection
before it removes the callback. The process retains ownership of pool closure.
Disabled metrics register no callback.

| Metric | Type and unit | Meaning |
| --- | --- | --- |
| `stego.db.pool.connections` | Gauge, `{connection}` | Current connections with fixed `state` values `used` and `idle` |
| `stego.db.pool.limit` | Gauge, `{connection}` | Configured maximum open connections |
| `stego.db.pool.waits` | Cumulative counter, `{wait}` | Pool wait count |
| `stego.db.pool.wait.duration` | Cumulative counter, `s` | Total wait duration reported by the pool |
| `stego.db.pool.connections.closed` | Cumulative counter, `{connection}` | Connections retired for `idle_limit`, `idle_time`, or `lifetime` |

There are eight fixed series per process. The resource carries the existing
service and instance identity. No database address, name, URL, user, SQL text,
Gateway ID, or caller-supplied label enters these metrics. A new process has a
new instance identity. Its counters describe its own pool.

These custom metrics preserve the meaning of
[Go's pool statistics](https://pkg.go.dev/database/sql#DBStats).
Wait duration is a total, not a distribution or the current number of waiting
callers. It must not be reported as the OpenTelemetry
[connection wait histogram](https://opentelemetry.io/docs/specs/semconv/db/database-metrics/).
Driver-call durations continue to exclude time spent waiting for a pool slot.

Independent checks use the real Go pool with a local test driver. They check
held and idle connections, wait cancellation, cumulative totals, retirement,
resource identity, disabled metrics, collector failure, and pool ownership.
Compiler checks cover SQL, GORM, absent pools, and unused database components.
The Hypershell acceptance change must verify these metrics through its existing
Gateway pool-pressure, access, event, recovery, and restart workflow. Full CI
and the application result remain required.

Full compiler CI
[34960363865](https://github.com/jsell-rh/stego/actions/runs/34960363865)
passed at `e8f16c7`, including race tests and SQL provisioning. The focused
runtime checks also passed with one local CPU and a 384 MiB Go memory limit.
They used a local test driver; no performance or PostgreSQL workload ran on
the workstation. Hypershell's expanded application check remains separate.

This covers the pool that generated service assembly owns. Separate worker
entry points and independent PostgreSQL clients still require their own
explicit resource wiring. These metrics do not establish production capacity.

The [Hypershell API result](hypershell-pool-api.json) passed at `916f3a7` in run
`34961014687`. All 31 required tests passed. Verification matched 899 source
files, 230 generated files, and all four generation records. The pool test
passed in 9.83 seconds. It verified two occupied connections, caller cancellation,
recovery, a new runtime identity after restart, and continued allowed and denied
Gateway access during collector failure. Cumulative waits increased from 9 to
13; total wait duration increased from 0.095924 to 3.971334 seconds. These are
test observations, not a performance target. The Job, Pods, and private fixtures
are absent. The shared Lease was free at the independent check.

The [complete browser pool test](hypershell-pool-browser.json) also passed at
`916f3a7` in run `34961014607`, in 366.3 seconds. All three checkpoints retained
the three-connection console limit and had distinct runtime identities. The
final checkpoint followed key rotation and collector recovery. Namespace and
SQL recovery, access rules, encryption, account cleanup, and automatic cleanup
passed. Verification matched 899 source files, 231 generated files, and all
generation records. Independent reads confirmed cleanup. Full core CI passed
in 1223.434 seconds; the overall run failed only on the CNPG and Sandbox jobs.

These application results use compiler `e5b9931`. They close the API and console
pool checks for this source. They do not verify the later PostgreSQL client
signal change or establish production capacity.
