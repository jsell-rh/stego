This component generates the per-service authorization wiring of a service
that binds authorization to SpiceDB, on top of the runtime library
stego.dev/runtime/authz. The ports, the SpiceDB provider, the consistency
floor, the relay, the revocation records and the handle are in the runtime
library.

From the declaration's authorization setting, its entities and its schema,
the component generates the package internal/authz with:

- Schema, the service's .zed schema with the floor marker definition
  stego_floor { relation marker: stego_floor } added, so the author never
  writes it, and the same text as schema.zed. Generation never writes the
  schema to the provider;
- Operations, the permission of each operation of each collection, or the
  reason of a public operation;
- one resource function for each protected entity, which builds the
  tenant-qualified resource ID through the runtime's NewResource;
- one tuple function and one write function for each entity with
  relationships, which enqueue the touches and deletes of a row through
  the handle in the row's transaction;
- Migration, which adds authz_token to each protected table, creates the
  revocation floor authz_floor and the revocation records with the
  runtime's Migration, and grants the outbox worker role UPDATE on
  authz_token only;
- NewRelay, which builds the relay with the owning table of each protected
  resource type and the delivery alert bound, runs its start-up check of
  the floor marker, and registers the destination authz on the outbox
  worker: idempotent, must land, with its state hook;
- Watch, which runs the late-revocation alert of the relay at the watch
  interval.

The settings are alert_bound (default 5s), the delivery alert bound of a
revocation, and watch_interval (default 1s), the time between two reads of
the oldest age of the authz outbox messages.

The component needs pooled tenancy with the tenant column tenant_id: every
resource ID at the provider is tenant-qualified, and the relay stores the
token on the owning row by tenant_id.

The stage is alpha. On the runtime data path the service's main makes the
authorization runtime with NewRuntime for the generated commands and queries,
and its migration job runs Migration and then WriteSchema before the service
starts. The service's main does not run the relay and the watch yet.
