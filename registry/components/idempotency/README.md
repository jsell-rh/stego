This component generates the per-service part of the inbound idempotency
key, on top of the runtime library stego.dev/runtime/idempotency. The key
table, the transaction wrapper, the canonical JSON fingerprint and Purge
are in the runtime library.

From the declaration's idempotency setting, the component generates:

- SchemaSQL, the key table DDL, which is the runtime's Migration;
- the retention, which is 24h when the declaration gives none;
- the fingerprint, which is body: the method, the path and the body;
- the three problem type URIs under the service's error_type_base;
- the create and patch operations of each collection that take the
  Idempotency-Key header. Each one is required when Required on names it,
  and accepted when it does not. A GET, PUT or DELETE operation ignores
  the key;
- one fingerprint function for each of these operations, over the body
  fields that the operation declares;
- the purge job, a background task that runs Purge once a minute.

When this component is in the service, the rest-api component puts the
Idempotency-Key header and its 400, 409 and 422 problem responses on each
create and patch operation of its OpenAPI document.

The stage is alpha. A collection create or patch is a generated command
(ST-SUP-RC20-OC37), so the wrapper is the one that command.Run calls. The
generated commands do not use the settings and the fingerprint functions
yet, and the conformance tests are not generated yet. Until the commands
use them, the compiler does not select this component.
