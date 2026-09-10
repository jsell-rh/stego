This component generates explicit PostgreSQL reads for external state checks.
Add `postgres-client` to the service archetype. It has no entity, storage-adapter,
controller, or Hypershell dependency. The application supplies `Options`, trusted
SQL, bound scalar arguments, and scan destinations to `ReadRow`.

The client opens one connection per call and closes it before return. It has no
pool or background worker. A call has a six-second limit, or the caller's shorter
limit. The session uses a five-second statement limit, a one-second lock limit,
`pg_catalog` as its search path, and read-only defaults. The application still
needs database permissions that match its task. Read-only defaults do not make
untrusted SQL safe: an authorized SQL statement can change session settings.
Do not accept query text from an untrusted caller.

Host, port, user, database, password, and certificate trust are explicit. TLS
verifies the host and has no plaintext fallback. Authentication requires SCRAM.
An optional literal IP route changes only the socket destination. `PGSERVICE`
is rejected, and other PostgreSQL environment values cannot select credentials,
files, another host, fallback TLS modes, or session parameters. A CA input must
contain only certificate PEM blocks. Private keys and trailing data are rejected.

A query is limited to 16 KiB. Up to 128 scalar arguments share a 64 KiB value
limit. Supported arguments are nil, booleans, `int`, `int16`, `int32`, `int64`,
`uint`, `uint16`, `uint32`, `uint64`, finite floating-point values, strings,
byte slices, and `time.Time`. There can be up to 32 non-nil scan
pointers. Each PostgreSQL protocol message is limited to 64 KiB. This is a
message limit, not a total result-byte limit. The operation deadline also applies
while the client consumes the response. `ReadRow` returns the first row, as
`pgx.QueryRow` does. Discard destinations after any error.

Returned `Error` values contain only an operation stage and a validated SQLSTATE.
They do not wrap server messages, queries, arguments, certificates, or passwords.
Use `errors.Is` for caller cancellation, deadline expiry, and `ErrNoRows`. The
client does not decide whether an authentication error permits repair or whether
a result proves cleanup. Those rules belong to the application.

Independent generated tests use an isolated PostgreSQL service behind a TLS
fixture. They check bound values, session policy, refused writes, oversized
messages, cancellation, certificate mismatch, authentication failure, no rows,
and recovery after rejected reads. They also test configuration under unrelated
PostgreSQL environment settings. The Hypershell workflow supplies the separate
operator and application evidence. Production throughput and pool management
are outside this component's current contract.
