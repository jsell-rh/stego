The generated PostgreSQL adapter requires verified TLS by default. This policy
belongs to STEGO. Applications supply the database address and trust roots.
They do not supply a transport wrapper.

On 2026-09-11, the user approved verified TLS by default with the explicit
loopback test exception below. This is the accepted transport policy.

Set `sslmode=verify-full` in the connection string. Supply `sslrootcert` when the
database uses a private certificate authority. The client checks the trust chain
and server name. It requires TLS 1.2 or later. The policy checks every configured
host fallback before it opens the pool. A failed TLS connection cannot select a
plaintext fallback. Modes `disable`, `allow`, `prefer`, `require`, and `verify-ca`
do not meet this policy.

The explicit test setting `STEGO_DATABASE_ALLOW_INSECURE_LOOPBACK=1` permits
plaintext only to literal loopback IP addresses. This includes `127.0.0.1` and
`::1`. It does not permit DNS names, Unix sockets, remote hosts, or unverified
TLS. Omit the setting, or set it to `0`, to require verified TLS. Other values
are configuration errors. Keep the exception out of production environments.
CI uses it for bounded local database fixtures. TLS tests explicitly set `0`.

Component version 4.0.0 changes the default. Existing applications that use
plaintext must configure verified TLS before they adopt the new output. The
generated event source copies the pool's validated pgx configuration for its
LISTEN connection. It retains the same certificate and fallback policy.

Configuration errors report `database.open`. Certificate failures during the
bounded startup probe report `database.ping`. The generated process excludes
driver errors, credentials, connection strings, and certificate paths from its
failure record.

The generated-code tests check the policy with and without telemetry. A real
TLS connection test checks a valid certificate, a wrong server name, and a
wrong trust root. The Hypershell test uses native PostgreSQL TLS. It creates a
Gateway and its owner grant, reads the event, and checks pool and LISTEN sessions
through `pg_stat_ssl`. It checks access and restart, and rejects unsafe modes
and invalid certificates. Hypershell contains no production TLS policy code.

The policy follows the pinned
[pgx configuration behavior](https://github.com/jackc/pgx/blob/v5.11.0/pgconn/config.go).
It validates the parsed connection targets. Strict connection-string key and
ambient-file controls, certificate rotation, independent provider connections,
and full production deployment evidence remain separate work. This change does
not close the wider enterprise goal.

The first Hypershell probe used compiler `eed6578`. Gateway creation, its owner
grant, event delivery, TLS session checks, and REST access checks passed. The
next start used `sslmode=require` with a trusted certificate for `localhost`
and the database address `127.0.0.1`. The application continued to run. The
external test deadline stopped it after three seconds. The package failed in
9.474 seconds. The initial application archive SHA-256 was
`12b65033f91b3444c32d9d2be2222e9e3a0334ee129ca30293ec7388164c8c28`.

On 2026-09-11, Job `db-tls` in namespace `stego-db-tls-20260911` ran the
compiler checks with Go 1.26.8 and PostgreSQL 18.6. The test container had one
CPU and a 3 GiB memory limit. PostgreSQL had half a CPU and a 512 MiB memory
limit. The job deadline was 30 minutes. No local build or performance test ran.

The complete adapter race suite passed in 91.691 seconds. Compiler tests passed
in 59.541 seconds and registry tests in 2.141 seconds. Static checks and the
compiler build passed. The tested compiler archive SHA-256 was
`b9e6f37ca785209ad31d3468bca8c01a0b4839f890abd9c77bffa96a6e2b4811`.
Later compiler edits changed documentation only. Full CI and pinned application
acceptance remain separate checks. These times do not measure request capacity.
