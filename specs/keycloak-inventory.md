# Bounded Keycloak inventory

This is an open requirement. The HTTP 202 Gateway workflow does not yet prove
large-inventory cleanup. Hypershell's current inventory reads realm-wide client
pages, then reads each client separately. Its five-second call budget can expire
on unrelated clients before account cleanup can finish.

## Provider evidence

The pinned Keycloak 26.7.3 API supports client-name searches and custom-attribute
queries. The list operation can omit a representation after a storage failure.
A short page is therefore not proof that a known provider object is absent.
See the [client API](https://www.keycloak.org/docs-api/latest/javadocs/org/keycloak/services/resources/admin/ClientsResource.html)
and the [pinned request implementation](https://github.com/keycloak/keycloak/blob/26.7.3/services/src/main/java/org/keycloak/services/resources/admin/ClientsResource.java).

The pinned JPA implementation uses a case-insensitive contains search for client
names. It passes the search value into a SQL LIKE pattern. A common literal-search
contract must handle wildcard characters explicitly. Custom-attribute queries
can require an operator allowlist. Results use offsets and client-name order;
they are not a stable snapshot or an immutable-ID cursor. See the
[pinned storage implementation](https://github.com/keycloak/keycloak/blob/26.7.3/model/jpa/src/main/java/org/keycloak/models/jpa/JpaRealmProvider.java).

## Required boundary

STEGO must own safe provider queries, response bounds, durable scan state, and
work scheduling. Hypershell supplies the client-name convention, ownership
attributes, account policy, and Gateway scope. Query filters select candidates;
they do not authorize mutation. Each candidate still requires the immutable
provider ID, current ownership check, and durable lifecycle closure.

Retained account IDs remain a separate source. A filtered inventory cannot
replace their checks or turn a partial provider response into proof of deletion.
A missing continuation, invalid response, denied query, or expired budget must
leave cleanup pending. Do not fall back to an unbounded realm scan.

A durable continuation must account for changes caused by cleanup itself. Saving
only an offset can skip later clients after earlier clients are removed. Repeating
only the first page can let failed clients block all later work. Before choosing
an implementation, prove bounded progress and repeated full coverage with small
fixtures that change between pages. Include provider restart, failed item retry,
foreign-name collisions, invalid ownership, late creation, and final-event
rollback. Then repeat the real Keycloak and Gateway workflows.

This requirement does not claim capacity from a small test. The existing live
Gateway and CNPG checks remain the acceptance gate for the deletion change.

## Query candidate

Branch `codex/keycloak-inventory-20260916` adds a bounded `SearchClients`
operation in provider version 0.14.0. It passes a client-name fragment through
encoded query values, with fixed search mode and explicit page bounds. It
rejects empty fragments and pattern characters before network access. A failed
query has no full-scan fallback. Focused generated tests passed in 3.087 seconds.
The real Keycloak gate checks case-insensitive filtering and offset handling.
Its result is still required before this provider change can enter the default
branch. Hypershell has not adopted this candidate. Durable inventory scans and
the required changing-page recovery tests remain open.

The real provider job in CI run `35099578328` passed at `f5d7d27`. Its complete
provider test took 62.31 seconds and includes the new search checks. Generated
source and runtime evidence are saved in `keycloak-search-real` under the
Gateway cleanup run directory. Both examples and PostgreSQL checks also passed.
The compiler job is still running. This does not qualify inventory cleanup or
Hypershell adoption.

All five CI jobs in run `35099578328` passed at `f5d7d27`. This qualifies the
bounded query, including real Keycloak behavior and both generated examples.
Later changes in this branch are documentation only. Inventory cleanup and
Hypershell adoption remain open.

## Retained journal omission

Hypershell regression `9f7d5cd` failed in 0.02 seconds against deletion source
`fd6371e`. A failed provider deletion left a sealed closure journal with a known
client ID. After client reconstruction, the provider list omitted that client.
Bulk cleanup returned success while the orphan remained. Direct cleanup with
the saved ID succeeded. This fixture proves a missing recovery source; it does
not claim a database or process restart test. Evidence is
`provider-journal-omission-probe.log` under the Gateway cleanup run directory.

The application currently scans retained account rows. A legacy orphan can have
a saved journal without an account row. STEGO must supply bounded resource-state
key enumeration by entity and scope, without returning ciphertext. It needs
stable key pagination and validation before database access. Hypershell selects
the Gateway scope. Account cleanup must also cover those journal IDs before it
can report completion. The new name query cannot replace that recovery source.
The final fix must prove the composed application completion condition, then
repeat provider and complete Gateway checks. Keep Hypershell's default branch
at its qualified revision until this defect is fixed.

## Save discovered cleanup targets before changes

The Hypershell partial-disable regression now requires a saved closure record
for each discovered client. Before this change, the second client's disable
failure left all three clients without a cleanup journal. The failing result is
saved in `inventory-registration-probe.log` in the Gateway cleanup run directory.

Provider 0.15.0 adds `PrepareCloseExisting`. It records irreversible closure for
an ownership-checked provider ID before any provider operation. It uses the
common lifecycle journal, validation, encryption, version checks, and writer
gate. It retains the subject and incomplete migration state. It does not claim
that access is disabled or that provider cleanup is complete.

Focused generated account and native lifecycle tests passed with the race
detector in both telemetry variants in 20.837 seconds. They include lost save
results, retry, changed IDs, invalid input, retained subject and migration,
reconstruction, and subsequent deletion. Evidence is
`prepare-closure-complete-focused.log`. Full CI and application adoption remain
pending. A complete provider scan before registration still needs a bounded
recovery design for large legacy inventories.
