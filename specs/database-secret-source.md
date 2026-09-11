The generated process can read its database URL from a mounted secret file.
This source selection and file policy belong to STEGO. The same code supplies
SQL and GORM startup, including registered pool factories.

Set exactly one nonempty setting: `DATABASE_URL` or `DATABASE_URL_FILE`. The
second setting must name an absolute path. If both settings are nonempty, or
both are empty, startup fails. The process never chooses one source silently.
It does not copy file contents into the environment or command arguments.

The file must be regular and no larger than 64 KiB. The environment value has
the same size limit. The file can have owner read and write access and group
read access. It cannot have execute bits, group write access, or any access for
others. For a projected Secret, use mode `0440` and the service's private volume
group. Only the containers that need the credentials should mount that Secret.

The reader follows symlinks to support projected Kubernetes Secret volumes.
It checks type, permissions, and size on the opened file descriptor. It uses
a nonblocking open so a FIFO cannot stall startup. The read limit still applies
if a regular file grows after the size check. Paths, file errors, and file
contents are excluded from the returned error and process failure record.
Configuration failures report the fixed `database.configure` stage.

The file contains one connection string. One final LF or CRLF is permitted.
Other newlines and NUL bytes are rejected. Credential spaces are preserved.
The adapter still checks transport policy and the startup probe still verifies
the connection. A file does not bypass verified TLS or startup deadlines.

The process reads the file once at startup. After a credential change, update
the projected file and restart the process. Existing pools do not reload their
credentials. The event listener receives the same validated connection settings
as the pool. The source policy does not impose a wall-clock deadline on arbitrary
filesystem operations; use local mounted secret files.

The volume behavior follows the
[Kubernetes Secret documentation](https://kubernetes.io/docs/concepts/configuration/secret/).
Mount the Secret directory, not a `subPath` file, when file updates are required.
The file reader does not depend on Kubernetes APIs, resource names, or Hypershell
types. Full deployment packaging, secret distribution, rotation without restart,
and strict DSN key controls remain separate work.

Compiler tests check source conflicts, file types, permissions, size limits,
line endings, private errors, projected-file replacement, and a real mounted
Secret when supplied. Generated SQL and GORM process tests check that both
sources reach the pool factory and retain bounded ping and cleanup behavior.
The Hypershell workflow creates a Gateway and owner grant, receives the event,
checks REST and gRPC access, changes the database password, replaces the secret,
and repeats reads and creation after restart. Bad sources must stop startup.

The first application probe used compiler `084bd76`. With only
`DATABASE_URL_FILE` set, the process exited at `database.configure` before it
opened an API listener. The package failed in 5.339 seconds. The initial
application archive SHA-256 was
`1040bd4147a45bae25d7c9f261e6368b2e417e67ab8ea213242390028293a053`.

On 2026-09-11, Job `db-secret` in namespace `stego-db-secret-20260911` ran
Go 1.26.8 and PostgreSQL 18.6. The test container had one CPU and a 3 GiB memory
limit. PostgreSQL had half a CPU and a 512 MiB memory limit. The job deadline
was 30 minutes. The mounted Kubernetes Secret used mode `0440` and a projected
symlink. No local build or performance test ran.

The complete compiler race suite passed in 63.456 seconds. This included the
generated file-source checks and SQL and GORM process checks. Static checks and
the compiler build passed. The tested compiler archive SHA-256 was
`670e2fc7eda047dcc9460a4e3a72ca2e49b688e4178bd9c5e8310a2d1c43a542`.
Later compiler edits changed documentation only. Full CI and pinned application
checks require separate results. These times do not measure request capacity.
