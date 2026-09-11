The generated PostgreSQL adapter owns connection-pool policy. Version 3.15.0
selects its pool factory with or without the telemetry component. The generated
main owns pool closure. Applications do not need a pool wrapper.

Deployment settings are read once when each pool is created:

| Environment variable | Default | Accepted value |
| --- | --- | --- |
| `STEGO_DATABASE_MAX_OPEN_CONNECTIONS` | 16 | Decimal integer from 1 through 1024 |
| `STEGO_DATABASE_MAX_IDLE_CONNECTIONS` | Smaller of 4 and the open limit | Decimal integer from 0 through the open limit |
| `STEGO_DATABASE_CONNECTION_MAX_LIFETIME` | 30m | Go duration from 1s through 24h |
| `STEGO_DATABASE_CONNECTION_MAX_IDLE_TIME` | 5m | Go duration from 1s through 24h |

An unset setting uses its default. An empty or invalid setting stops pool
creation. Integer settings accept at most four digits and reject signs, fractions,
whitespace, and overflow.
Errors identify the setting name but exclude its value and connection details.
The limits cannot be disabled with zero, except for idle connections. An idle
limit of zero closes connections after use. The lifetime and idle-time settings
apply independently. Connection retirement does not interrupt active work.

These defaults provide a finite per-process budget. They are not a production
capacity estimate. Operators must budget all replicas, the separate event
listener, external database clients, migrations, and administrative connections
against the database server's connection limit. The default does not reserve
server capacity or limit the number of waiting requests.

The pool uses Go's `database/sql` connection management. An operation waits when
all allowed connections are in use. A caller's context can cancel that wait.
A transaction must use its retained transaction connection. A callback that
holds every connection and then acquires another can deadlock. See the
[Go connection-management contract](https://go.dev/doc/database/manage-connections).

The application gate holds two Gateway reads behind a PostgreSQL table lock.
A third request must wait and retain its deadline without creating an extra
pool connection. The first reads must recover after the lock is released.
The same workflow checks an atomic owner grant, event delivery, denied access,
and a read after restart. The test excludes the separately owned LISTEN session
from the pool count. It uses at most three simultaneous API requests.

Independent generated-code tests cover invalid configuration, fixed errors,
small-pool waits, deadlines, release, idle counts, and connection retirement.
They run with and without telemetry. Compiler checks cover the factory wiring
and generated-name conflicts. Application and compiler results must be recorded
separately before this policy is treated as verified.

Startup connection deadlines, automatic startup migrations, per-request work
budgets, connection-pool metrics, separate PostgreSQL clients, and production
capacity evidence remain open. The database telemetry profile still excludes
pool wait time from driver-call duration.

The Gateway probe on compiler `5c0dd1f` failed with three pool connections when
two were requested. The test reached that assertion after Gateway creation,
owner-grant, event-delivery, and owner-read checks. It did not reach the later
recovery checks. Its source archive hash was
`09bcf77006fc8fc086f3fe54c8753a9f6eacab38ee04c71561299e43d1ae7b28`.

The revised compiler snapshot passed all PostgreSQL adapter tests under race
detection in 70.805 seconds. This includes generated pool tests with and without
telemetry and the storage transaction suite. Compiler tests passed in 55.306
seconds, registry tests passed in 2.164 seconds, and static analysis passed.
The command returned exit code 0. Its source archive hash was
`b2a0cb0f161e7e4b352a44c2c4498cdb3e89feeb7921c9146a84d18c362a4701`.
The checks used Go 1.26.8 and PostgreSQL 18.6 in Job `pool-workflow`, namespace
`stego-pool-20260911`. The test container had one CPU, 3 GiB of memory, and
`GOMAXPROCS=1`. PostgreSQL had half a CPU and 512 MiB. The Job had a 30-minute
deadline and no mounted service-account token. PostgreSQL listened on Pod
loopback only. Full CI and application adoption require separate results.
