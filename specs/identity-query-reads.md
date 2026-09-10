# Identity reconciliation reads

Hypershell must read a user's current identity and Gateway grants before it
changes provider roles. The read must distinguish a known removal from a missing
or denied response. A deleted user still needs its stored issuer and subject so
that the controller can remove roles from the correct provider user.

The application used counted lists for user and role lookups, and counts for
grant existence. A PostgreSQL regression test found six reads, including three
counts, for an owner. A viewer or removed role required nine reads, including
five counts. A deleted user required three reads, including one count. None of
these totals is part of the controller response.

The application now uses STEGO's existing CursorReader with a limit of one.
The user lookup includes retained deleted rows. Role and grant lookups include
only live rows. User IDs and role names must match exactly. A second matching
role is an error. Grant existence keeps the Gateway, user, role, and scope
filters. Owner precedence and the transaction boundary stay the same.

The shared role lookup also serves Gateway creation, access checks, and global
role synchronization. It now requires the generated cursor capability. An
adapter without that capability returns an error. The test transaction wrapper
for a concurrent Gateway update forwards the capability to the real adapter.
It still pauses the write to test the original race.

The focused race tests passed in 2.292 seconds. They check owner precedence,
viewer access, removed grants, a grant for another Gateway, deleted users,
unbound identities, absent users, missing role definitions, denied callers,
and a concurrent Gateway update. The query recorder retains counts only.
Owner reads now use four queries. Viewer and removed-role reads use six.
Deleted-user reads use two. These paths run no count query. Denied callers run
no read. A missing role definition returns an error without identity state;
it does not authorize provider cleanup.

Three before-and-after benchmark samples used 100 owner-state calls each, with
10,000 unrelated users and grants and current PostgreSQL statistics. The host
used Go 1.26.8, PostgreSQL 18.6, Linux amd64, and an Intel Core Ultra 9 185H.

| Measure | Before | After |
| --- | --- | --- |
| Time per call | 979,480–1,121,498 ns | 817,026–855,064 ns |
| Allocated bytes per call | 50,094–50,130 | 55,136–55,493 |
| Allocations per call | 663 | 680–681 |

The local samples show fewer database queries and lower elapsed time, with
higher allocation cost. Cursor reads include a lookahead row and result
validation. These measurements exclude gRPC, Keycloak, controller scans, and
concurrent load. They do not establish a production capacity limit.

No new STEGO primitive or Hypershell-specific generator was needed. STEGO owns
query construction, bound parameters, limits, deletion visibility, and result
validation. Hypershell owns grant precedence, access policy, and the identity
sent to its provider.

This change does not replace the identity inventory's numbered pages or make
its progress durable. Durable retries, memory bounds for incomplete scans,
multiple-controller fencing, and immediate revocation of issued tokens remain
separate requirements. The full enterprise goal remains active.

The full query-change application race suite passed 116 acceptance tests in
855.347 seconds with PostgreSQL and Keycloak required. Its three Kubernetes tests
use a separate gate and were skipped in that run. Static checks and module
verification passed. The first Gateway cluster gate exposed the
[generated gRPC header defect](grpc-stream-headers.md). The compiler fix required
no further query-source change.

Hypershell commit `4c96d4f89c93f63e0d7a497ef7faa15aa0c59761` contains the query
change. Commit `2ddb2e4a1f9e8b65eda48d128060eb8d35519d9d` pins the gRPC fix and
adds an application regression through the generated TLS client. With that
compiler, login, grant changes across transports, restart, and the concurrent
update check passed in 38.892 seconds. The complete Gateway cluster gate passed
in 209.690 seconds. Final controller and contract tests passed, and the final
query checks passed in 1.986 seconds. All 231 Go source and dependency files in
the main checkout matched the fixed-compiler test checkout. Post-commit generation
preserved all 74 generated and dependency file hashes. Both commits are on remote
main. See the [application evidence](https://github.com/jsell-rh/hypershell-stego/blob/2ddb2e4a1f9e8b65eda48d128060eb8d35519d9d/acceptance/identity-queries.md).
