# Resource state for provider recovery

Provider recovery can require a saved client ID and a migration checkpoint
before the next external write. A scan cursor cannot represent this state.
STEGO supplies a versioned record and a separate encryption codec. Application
policy must still authorize the resource and choose a fixed scope.

## Storage contract

`ResourceStateStore` stores one binary record per exact entity, resource ID,
and scope. The limit is 65,536 bytes. Version zero means absent. Saves require
a transaction and the version read before work. A conflict does not retry the
callback or a provider action. An ignored save error prevents transaction commit.
Saving empty data clears content and retains the version. A database trigger
rejects removal of history, changes to record identity, and version changes
other than an increment of one. This is not a provider lease or an access grant.

PostgreSQL adapter 4.5.0 adds `000009_resource_state.sql`. Apply this migration
before an externally migrated application starts. Store startup checks required
columns, the exact primary key, bounds, row-security state, and the history
trigger. Keep schema ownership separate from application credentials. The raw
storage interface accepts opaque bytes; it does not encrypt them.

## Protected records

Controller runtime 1.18.0 adds `StateProtector`. Supply one active random 32-byte
key and at most three older read keys. Keep these keys outside the state database.
A password is not a key. The constructor rejects wrong key lengths, an all-zero
key, duplicate keys, and more than four keys. It copies the supplied keys.

`Seal` accepts a trusted instance, entity, resource ID, scope, positive record
version, and at most 65,459 plaintext bytes. Each operation uses a fresh random
256-bit salt. HKDF-SHA-256 derives a separate AES-256 key from the master key,
salt, and record context. The Go library generates the GCM nonce. This avoids a
shared AES key across all record writes. The envelope has this fixed layout:

| Field | Bytes |
| --- | --- |
| Format version, value 1 | 1 |
| Master-key ID, first 16 bytes of SHA-256 | 16 |
| Random HKDF salt | 32 |
| GCM nonce | 12 |
| Encrypted content | 0 through 65,459 |
| GCM authentication tag | 16 |

The authenticated context contains the format domain, length-prefixed instance,
entity, resource ID, scope, record version, and envelope header. A changed key,
version, header, or ciphertext fails authentication. Unknown keys and formats
fail. `Open` returns a protected value that requires explicit `Reveal` to obtain
a copy. Keys and plaintext reject implicit JSON export and use redacted output
for every formatting verb. Error messages contain no keys, data, or record IDs.
The codec uses the standard [Go HKDF](https://pkg.go.dev/crypto/hkdf) and
[authenticated encryption APIs](https://pkg.go.dev/crypto/cipher#NewGCMWithRandomNonce).

To save confidential state, load the current record, seal for its next version,
and save with the current version as the comparison value. Commit before the
external write. On a conflict, load current state and decide the next action
again. Do not replay the external write from a transaction callback. To rotate
keys, make the new key active, retain old read keys, and re-encrypt records with
new record versions. Remove old keys only after all required records and backups
have a supported recovery path.

Encryption cannot detect restoration of an entire database to an older valid
snapshot. Database restore and provider recovery need an explicit operator
procedure. These APIs do not by themselves provide that procedure, key mounting,
key rotation automation, or provider ownership policy.

## Evidence and remaining work

The generated storage harness passed local compilation, vet, and its tests in
5.008 seconds, with database tests skipped.
CI must prove binary round trips, store replacement, concurrent registration,
stale writers, retained empty records, rollback after ignored errors, and
schema rejection. The encryption tests cover all envelope-byte changes,
record-context changes, version changes, key rotation, bounds, copies, redaction,
and concurrent use. Both generated variants passed with the race detector in
6.933 seconds. The PostgreSQL adapter suite then passed with the race detector against real
PostgreSQL in [CI run 35037362602](https://github.com/jsell-rh/stego/actions/runs/35037362602),
in 82.068 seconds. The full run failed because two compiler assertions and the
checked-in examples still described the prior storage version. Those failures
require separate correction; the full run is not a pass.

Hypershell commit `a2392a0` adopts these APIs for its private Gateway identity
recovery record. The controller retains its encryption key; the API applies
Gateway permissions and limits records to 60 KiB to fit the generated RPC bounds.
Its new REST, TLS gRPC, restart, and cleanup test is queued in CI. The production
Keycloak controller does not yet use the record. Next, connect the common
provider journal and lifecycle before ownership migration, then remove the
remaining application scope, mapper, and client enablement mechanisms.
