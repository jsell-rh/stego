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
repeat-build review. Its [fix record](postgres-invalid-drop-fix-evidence.json)
remains separate from the later signed release result.

## Signed release

The exact source passed all six main jobs in
[run 35501922557](https://github.com/jsell-rh/stego/actions/runs/35501922557).
Independent review verified all 34 race-tested packages, both generated
examples, and all 17 focused recovery cases. No focused case was skipped.

The immutable [signed compiler release](https://github.com/jsell-rh/stego/releases/tag/compiler-3be6bce4167c54258bb4c154ad2cfb5e7e0d00d0)
passed source, module, toolchain, build-policy, signature, and installation
checks. The review matched 1,307 source files and 28 module records. Two isolated
builds agreed, and the signed main binary matches the qualified branch binary.
The review did not execute the downloaded compiler locally. See the
[release record](postgres-invalid-drop-release-evidence.json).

Hypershell must select this verified package, regenerate, and pass its complete
application workflow. The constructed catalog-state test does not establish
physical crash recovery or whole-database rollback detection.
