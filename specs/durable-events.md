STEGO must preserve a database write and its notifications through a process
failure. It must not return a failed-write response because a notification failed
after the write committed.

Database rules and notifications have different execution requirements. An owner
grant must commit with the resource. A message to Kafka or an external service
must use a durable handoff. The proposed callback contract separates these
operations. The user has been asked whether existing notification callbacks can
move to asynchronous execution. The queue implementation does not depend on that
answer.

The first implementation generates a PostgreSQL queue and an explicit SQL
migration. It is not yet registered as a complete service capability. Kafka
delivery, callback dispatch, storage integration, and migration management remain
required work.

The queue has these rules:

- The application supplies its SQL transaction. Enqueue the complete notification
  batch once, then commit only if all database rules and queue writes succeed.
- A message has a stable ID, destination, resource key, kind, and JSON payload.
  The ID identifies one delivery target and remains unchanged across retries.
- The payload is an explicit application value. Do not copy a complete resource,
  credential, or identity into it by default. Payload validation rejects duplicate
  JSON members, invalid UTF-8, excessive nesting, and inputs over 64 KiB.
- Each batch contains at most 32 messages. Advisory transaction locks are acquired
  in a stable order before sequence allocation. Concurrent transactions for the
  same destination and resource key cannot reverse their delivery order.
- Workers claim pending messages with row locks and `SKIP LOCKED`. Each claim has
  a random lease token. Acknowledge and retry require the current, unexpired token.
- A failed message remains stored. Its retry delay blocks later messages for that
  destination and resource key. Other keys can progress. Operator tools must make
  blocked messages visible before this feature is ready for production.
- Delivery is at least once. A worker can send a message and stop before it saves
  the acknowledgement. Consumers must use the stable ID to prevent duplicate
  effects. The queue does not claim exactly-once delivery to an external service.
- SQL operations have a five-second deadline. Lease duration is between one
  second and five minutes. The worker bounds each delivery attempt to less than
  its lease duration.

The generated worker runs a fixed number of delivery tasks. Each task claims one
message when it is ready to deliver. Defaults are four tasks, a 250 ms idle poll
interval, a ten-second attempt deadline, and a 30-second lease. Configuration
checks require enough lease time for both database operations and delivery.
Retries use exponential delay with jitter and a configured upper bound. Unknown
destinations remain queued with a fixed failure code.
Repeated database failures also increase the retry delay. With the default
configuration, that delay is capped at five seconds and includes jitter.

Cancellation stops new claims and cancels active delivery contexts. The worker
waits for the handlers to return. A completed delivery can still be acknowledged
during shutdown. This database operation has a five-second limit. Handlers must
honor cancellation; the worker does not start detached goroutines to conceal a
handler that fails to stop. A result returned after an attempt deadline is not
acknowledged as success.

Worker counters report delivery, retry, database failure, unknown destination,
and lease-loss totals. They do not contain payloads or raw error messages. The
queue stores short failure codes. These counters are not yet connected to the
service telemetry or readiness endpoints.

Claims preserve order for each resource key. External sinks can observe repeat
deliveries after lease loss or an uncertain acknowledgement. Use the stable ID
to prevent duplicate effects. Sinks that enforce resource versions must also
reject an older version after a newer one.

The SQL migration creates a dedicated schema and table. The queue constructor
does not change the database. Production deployment still needs explicit
migration versions, separate database permissions, queue monitoring, retention
rules and tested Kafka security settings.

The generated runtime tests use private databases on PostgreSQL 18.6. They check
transaction commit and rollback, duplicate IDs, ordered retries, concurrent
workers, stale acknowledgements, canceled operations, and abrupt process exits.
CI requires PostgreSQL for these tests. Tests never drop a caller's database;
they create and remove their own randomly named databases.

A local benchmark used Go 1.26.8 on Linux amd64, an Intel Core Ultra 9 185H, and
PostgreSQL 18.6 in Docker over a local TCP connection. For 100 messages, one claim
and acknowledgement pair took 2,461,049 ns/op, 6,089 B/op, and 137 allocations/op.
This is a small-queue baseline. It does not establish service throughput, large
backlog behavior, or Kafka performance. Run it with `STEGO_BENCH_OUTBOX=1` and
`STEGO_TEST_POSTGRES_DSN` through `go test -v ./internal/generator/outbox`.
