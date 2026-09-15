# Durable effect bindings

The PostgreSQL adapter 4.4.0 generates `EffectBindingStore`. It stores one public
SHA-256 digest for each exact entity name, resource ID, and application scope.
Use this record when an external effect needs retained state, such as encryption
keys. The digest is not a credential. Do not store secret values in this table.

Before the first external use, authorize the caller and lock the live resource
in a generated storage transaction. Call `BindEffect` in that transaction. Do
not use the state until commit succeeds. An unknown commit result requires a
read or a safe retry before an external operation. The same digest can be
registered again. A different digest is rejected. The application must use a
fixed, bounded set of scope names and prevent resource identity reuse.

After durable, irreversible deletion, call `CloseEffectBinding` in a generated
transaction. Closure prevents later registration and retains any accepted
digest. A closed record with an empty digest proves that no registration
committed. It does not prove that arbitrary external resources are absent.
A closed record with a digest requires the original state and cleanup checks.
Missing state must stop cleanup. Absence alone is not a cleanup result.

The store does not cancel a previously authorized external operation. The
provider must prevent late work from restoring deleted resources. For example,
the generated PostgreSQL provisioning runtime retains its own deletion record.
This protocol also requires that no old worker can bypass registration.
Applications that add this protocol must reject incompatible schema generations
or supply a separately reviewed migration. An empty new table is not evidence
that an old installation has no external state.

`BindEffect` and `CloseEffectBinding` require an active transaction. Failure
prevents that transaction from committing even if the caller ignores the error.
The caller handles serialization failures; the store does not replay application
code. Reads and writes have the generated transaction time limit. The primary
key permits bounded exact lookup without a resource scan.

IDs and entity names have a 256-byte limit. Scopes have a 128-byte limit.
Values must be valid UTF-8 without NUL. Digests have exactly 64 lowercase
hexadecimal characters. Keys use bytewise, case-sensitive comparison.

Migration `008_effect_bindings` creates the table and history guard in the
existing migration transaction. It sends each SQL command separately so that
prepared statements remain supported. Startup checks the columns, constraints,
primary key, and guard. Row security or a disabled guard prevents startup.
The guard rejects row deletion, key or digest changes, and reopening a closed
record. Runtime roles must not have DDL, TRUNCATE, or trigger-control rights.
The guard does not protect against a database administrator.

The generated storage suite checks early closure, repeated registration,
retained state after store replacement, concurrent registration and closure,
transaction rollback, invalid inputs, and direct attempts to change history.
The suite also runs the migration with prepared statements enabled. Cluster
results and source hashes are recorded in `effect-bindings-evidence.json`.
