The PostgreSQL store now supplies an explicit transaction scope:

```go
err := store.WithTransaction(ctx, func(ctx context.Context, tx *storage.Store) error {
    if err := tx.Create(ctx, "Record", record); err != nil {
        return err
    }
    return tx.Notify(message)
})
```

`Notify` is generated when the component context includes the `outbox` peer.
The example uses application-supplied record and message values. No resource
payload is copied to an event by default. The queue is not yet registered in
the CLI. Generated HTTP handlers do not yet use this scope.

The scope owns one serializable PostgreSQL transaction. Create, read, replace,
upsert, delete, and list use the supplied transaction store. Domain code can
perform more than one store operation in the callback. It must use this store
and return operation errors. It must not retain the store or start work that
continues after the callback returns.

The callback receives a context with a ten-second deadline, or the caller's
shorter deadline. SQL work uses the same transaction deadline. Callback code
must honor cancellation. Arbitrary callback code that ignores cancellation can
still prevent return. The scope does not start a detached callback goroutine.

Notifications are staged in memory. Each message is validated and its payload
is copied. All calls form one batch of at most 32 messages, each with a payload
of at most 64 KiB. An invalid notification prevents commit even if the callback
ignores its returned error. The complete batch is passed to the queue once,
after the callback succeeds. Queue failure rolls back all resource changes.
This preserves the queue's common lock order across the complete batch.

Commit occurs only after the callback, cancellation check, and queue insert
succeed. Callback errors, invalid messages, missing queue schema, database
errors, and panics cause rollback. The scope does not recover application
panics. Nested store scopes and stores built from an existing GORM transaction
are rejected. Direct SQL transactions and GORM prepared-statement transactions
are supported. Other connection wrappers fail before the callback runs.

The callback is not replayed after a serialization failure. Application code
can have effects that cannot be repeated safely. Request retry and idempotency
policy still require service-level contracts. A failed commit can have an
uncertain outcome after connection loss. This scope does not claim exactly-once
request execution or external delivery.

Generated runtime tests use PostgreSQL 18.6 and the race detector. They check
atomic commit and rollback, uncommitted visibility, copied payloads, limits,
invalid messages, missing schema, retained scopes, nested transactions, prepared
statements, cancellation, panic rollback, and serialization failure without
callback replay. A blocked SQL query checks the deadline. Replace, upsert, and
delete checks prove that these methods remain in the same transaction.

A local benchmark used Go 1.26.8 on Linux amd64, an Intel Core Ultra 9 185H, and
PostgreSQL 18.6 in Docker over a local TCP connection. For 100 transactions,
create plus one notification took 1,711,180 ns/op, 17,444 B/op, and 230
allocations/op. This is a small local database baseline. It does not establish
service throughput or performance under contention. Run it with
`STEGO_BENCH_STORE=1` and `STEGO_TEST_POSTGRES_DSN` through
`go test -v ./internal/generator/postgresadapter -run '^TestGeneratedStoreTransactions$'`.

Shared public storage contracts, domain rule injection, HTTP and gRPC use,
explicit migration management, and complete Kafka service composition remain
open work. These are required before this scope is a complete service feature.
