The CNPG Gateway workflow required two external SQL reads: one to verify current
role permissions and one to prove cleanup. Both reads needed the same connection
identity, TLS rules, environment isolation, time limits, and error policy. That
code belongs in STEGO. CNPG resource ownership and interpretation of SQL results
remain in Hypershell.

`postgres-client` 1.0.0 generates a standalone `ReadRow` operation. It requires no
entity, storage adapter, controller, or application type. It uses one connection
per call. The caller supplies an explicit host, port, user, database, password,
certificate trust, trusted SQL, bound scalar arguments, and scan destinations.
An optional literal IP route preserves the host used for certificate verification.

The operation has a six-second limit, or the caller's shorter deadline. It closes
the connection with that same bounded context. Its session uses read-only defaults,
a fixed catalog search path, and statement and lock limits. These defaults do
not make untrusted SQL safe. Applications must keep query text in trusted code
and apply suitable database permissions.

The client rejects implicit service selection and prevents unrelated PostgreSQL
environment settings from selecting files, credentials, hosts, session settings,
or plaintext fallback. Certificate input accepts certificate PEM blocks only.
Query, argument, destination-count, and protocol-message limits are explicit in
[the component contract](../registry/components/postgres-client/README.md).
The message limit is not a total response-byte limit. There is no connection pool
or retry loop in this component.

Errors carry an operation stage and a validated SQLSTATE. Server messages, query
text, arguments, and credentials are not wrapped or returned. Cancellation,
deadline expiry, and no-row results support `errors.Is`. Hypershell decides which
connection errors justify operator repair; the generated client does not.

Independent generated tests use PostgreSQL through a TLS fixture. The actual
service performs SCRAM authentication. Tests verify bound values, read-only
session state, rejection of a write with SQLSTATE 25006 and no created table,
oversized response rejection, shorter caller deadlines, certificate mismatch,
wrong-password rejection, no-row handling, and recovery after failed reads.
Configuration tests cover unrelated environment values and invalid identities.
The full compiler race suite with required PostgreSQL and `go vet` passed.

The application uses this client for both live role checks and retained cleanup
checks. Its full CNPG workflow is the integration gate. Performance at production
load, connection pooling, general query pagination, and multi-process provider
ownership remain separate requirements.

Compiler commit `315afa13ccffd501c568c53d35e168febd650937` passed
[CI run 34510884026](https://github.com/jsell-rh/stego/actions/runs/34510884026).
The complete Hypershell CNPG Gateway workflow passed with race detection in
362.168 seconds using the generated client. It retained the application checks
for data and key isolation, restart, role and password repair, denied cleanup,
and late SQL role recovery. Provider race tests, contracts, CLI build records,
and static checks also passed. Regeneration preserved all 84 generated and
dependency file hashes. Application commit
[`0a846344d15dfedb5c8f870431d2ed042e55049e`](https://github.com/jsell-rh/hypershell-stego/commit/0a846344d15dfedb5c8f870431d2ed042e55049e)
contains the integration. The workflow timing includes provisioning and faults;
it is not a SQL performance benchmark.
