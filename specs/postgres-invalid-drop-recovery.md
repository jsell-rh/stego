# Recovery from an invalid PostgreSQL database

PostgreSQL marks a database invalid before some irreversible parts of removal.
An interrupted drop can leave this marker in its catalog. The server denies
connections and rejects `ALTER DATABASE` for this state. It permits another
`DROP DATABASE`. See the [PostgreSQL 18.6 implementation](https://raw.githubusercontent.com/postgres/postgres/REL_18_6/src/backend/commands/dbcommands.c)
and the [documented invalid marker](https://www.postgresql.org/docs/18/catalog-pg-database.html).

The common provider previously ran `ALTER DATABASE ... ALLOW_CONNECTIONS false`
before each drop. That operation prevents recovery when the database is already
invalid. The new provider reads the connection limit after its existing ownership
checks. It skips the alter operation only for the exact invalid marker, `-2`.
It still drops the database, removes both roles, and then records completion.
All other database states retain the connection-denial operation. Missing objects
retain the existing repeat-safe cleanup path.

Server identity, resource locking, recorded role and database IDs, ownership,
terminal deletion state, and caller deadlines remain required. An invalid marker
does not confer ownership and does not mean deletion is complete. The provider
does not write PostgreSQL system catalogs.

The regression uses a dedicated disposable PostgreSQL 18.6 server. Its fixture
constructs the catalog state left by an interrupted drop. The generated provider
runs as a non-superuser. The test requires cleanup through a new provider session,
confirmed database and role absence, a terminal ledger record, and repeat-safe
deletion. Separate cases require rejection of an invalid database with a foreign
owner or a different database ID. They check that denied cleanup preserves the
object and recorded state. Unrelated application data must remain unchanged.

The catalog injection is test code only. It does not test physical crash timing
or force a real checkpoint cancellation. Those claims require separate evidence.
Focused and full hosted checks qualified source 3be6bce for promotion. Signed
release and consumer adoption require separate checks. Component version is 1.5.1.

Regression run [35501332715](https://github.com/jsell-rh/stego/actions/runs/35501332715)
used the unchanged provider template from compiler 2084c32. The owned invalid
database case failed with SQLSTATE 55000 at the alter operation. Both ownership
denial cases and all 11 other selected cases passed. No case was skipped.
The server container was removed. The [regression record](postgres-invalid-drop-regression-evidence.json)
contains source and result hashes. This expected failure does not qualify a
compiler. The fix retains the exact regression test bytes.


## Fix qualification

Exact source `3be6bce` passed all six compiler jobs in
[run 35501449244](https://github.com/jsell-rh/stego/actions/runs/35501449244).
All 34 race-tested packages and both generated examples passed independent
result review. The focused [recovery run](https://github.com/jsell-rh/stego/actions/runs/35501449255)
passed all 17 cases with no skips. It retained the exact failing regression
bytes, all prior passing cases, both ownership controls, repeat deletion,
unrelated data checks, and private telemetry checks.

The unsigned branch build passed source, module, toolchain, build-policy, and
repeat-build review. This does not qualify a signed release. The exact source
is now on main, where new release checks are running. Hypershell must select
the verified signed package, regenerate, and pass its application checks.
See the [fix record](postgres-invalid-drop-fix-evidence.json).
