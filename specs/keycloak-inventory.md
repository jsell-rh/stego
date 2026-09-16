# Bounded Keycloak inventory

This is an open requirement. The HTTP 202 Gateway workflow does not yet prove
large-inventory cleanup. Hypershell's current inventory reads realm-wide client
pages, then reads each client separately. Its five-second call budget can expire
on unrelated clients before account cleanup can finish.

## Provider evidence

The pinned Keycloak 26.7.3 API supports client-name searches and custom-attribute
queries. The list operation can omit a representation after a storage failure.
A short page is therefore not proof that a known provider object is absent.
See the [client API](https://www.keycloak.org/docs-api/26.7.3/javadocs/org/keycloak/services/resources/admin/ClientsResource.html)
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
