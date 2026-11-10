This component generates the per-service part of the inbound idempotency
key. The key table, the transaction wrapper, the canonical JSON
fingerprint and the purge job are in the runtime library
(stego.dev/runtime/idempotency).

From the declaration's idempotency setting, the component generates:

- the migration of the key table idempotency_keys, and its text as
  SchemaSQL;
- the retention, which is 24h when the declaration gives none;
- the fingerprint, which is body: the method, the path and the body;
- the three problem type URIs under the service's error_type_base;
- the create and patch operations of each collection that take the
  Idempotency-Key header. Each one is required when Required on names it,
  and accepted when it does not. A GET, PUT or DELETE operation ignores
  the key.

The stage is alpha. The migration text is a copy of the runtime's
Migration until the generated service pins the runtime module, and a test
keeps the two equal. The wrapper call on the routes, the purge job, the
OpenAPI header and the conformance tests are not generated yet: they wait
on the runtime pin and on the decision of ST-SUP-Q66. Until then, the
compiler does not select this component.
