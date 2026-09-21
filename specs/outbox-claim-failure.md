# Failed outbox claim results

`Queue.Claim` returns no deliveries when its query, row scan, or row iteration
fails. A partial batch is not a successful claim result. The queue retains the
underlying error for the caller's error handling.

The database can have stored a lease before the client receives a complete
result. An error must not clear that lease. Another caller must wait until the
lease expires. Recovery retains the message IDs and contents, increases the
attempt count, and assigns a new receipt token. The old token must not remove
the new claim. These rules preserve the existing delivery and lease contract.

The tests use a bounded SQL driver around a real PostgreSQL connection. It
executes the claim, then returns a query error or a row error after one row.
This models an unknown client result. It is not a network protocol fault test.
The recovery cases wait for normal lease expiry; they do not edit lease times.

A mutation check restores the former partial-batch return in a generated test
copy. The specific partial-result regression must fail. A compiler error or an
unrelated test error cannot satisfy that check.

The Hypershell event timeout recorded in
[event restart evidence](event-restart-failure-evidence-20260921.json) remains
unexplained. The worker already ignores a claim result when an error is present.
This change does not claim to remove that timeout or change the recovery limit.
Application adoption and complete workflow checks remain separate requirements.

The immutable compiler release at `65b18de8` passed all 30 generated cases,
37 compiler packages, both complete generated examples, and the database access
check. The mutation check rejected the former partial-result behavior. Independent
signature checks and a fresh installation matched all five verified files.
See the [release evidence](outbox-claim-release-evidence.json).
Hypershell adoption of this queue change is still pending.
