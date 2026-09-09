# Desired generations and observation groups

`postgres-adapter` 3.6.0 extends versioned entities with declared inputs and
observation groups. The names and fields belong to the application domain.

```yaml
entities:
  - name: Task
    versioned: true
    generation_fields: [command]
    observations:
      worker: [phase, status]
    fields:
      - {name: command, type: string}
      - {name: phase, type: string, optional: true, unobserved: Pending}
      - {name: status, type: string, optional: true}
```

`ResourceVersion` changes on every write. `ResourceGeneration` starts at 1 and
changes when a declared input value changes or deletion begins. Rewriting the
same input does not advance the generation. Auxiliary writes and observations
advance the revision but do not advance the generation. PostgreSQL applies these
rules to ordinary writes and raw SQL. Caller-supplied generation values have no
effect. JSON inputs use structural equality; SQL NULL and JSON null are distinct.

Observation fields must be optional. They cannot have defaults, computed values,
or fills. A field cannot belong to more than one group or also be an input.
Unknown and repeated fields fail validation. Observation fields cannot be
collection upsert keys or patchable fields. Ordinary generated model writes
exclude them. An explicit application policy must reject attempts by callers
who do not have observation authority.

Call the generated `ObservationWriter.ObserveIfVersion` with the revision read
before external work, a declared group, and exactly that group's fields. The
method decodes the fields to their generated Go types, checks the live row and
revision, writes the fields, and records the current generation for that group.
It rejects unknown groups, extra or missing fields, invalid field types, and
payloads above 1 MiB. Binary and timestamp fields retain their typed values.
The caller must authorize the group and insert events in the same transaction.
A conflict requires a new observation. It must not be retried with an old result
and a newer revision.

The generated `ObservedGeneration(group)` method returns zero for an unknown,
absent, or invalid group. The generated `CurrentObservations()` method returns
a copy with stale observation fields set to null. A string observation field can
declare an `unobserved` value, such as `Pending`. This value must satisfy the
field constraints. It changes presentation; it does not create an observation.
Explicit facades and readiness checks must use this method when they read a
single row. The model retains older observation values for diagnosis. A desired change does not erase those values or claim that a new
observation exists. A successful write by one group cannot mark another group
current. A repeated status value can confirm a new generation after fresh work.

Generated lists use the same current values before scopes, filters, text and
structured search, related filters, sorting, and counts. Thus a search for a
successful status cannot return a stale success whose displayed status is
pending. Direct storage `Get` retains the original values for controller work
and diagnosis. A list is a current-state view, including a list with selected
fields. SQL literals preserve declared values with either PostgreSQL string
escape setting.

The automatic `rest-api` component does not yet implement this policy. It rejects
collections with observation groups. Explicit application facades must authorize
writes and present stale observations correctly. Hypershell uses this boundary
for its reference REST and gRPC shapes. Support for automatic CRUD APIs remains
required work; do not interpret this boundary as full compiler support.

## Migration

Use `Migrate` or the generated `000003_resource_generations.sql` upgrade. The
`000002_resource_versions.sql` artifact also contains the complete current
revision contract for new installations. Repeating the current migration leaves
revisions and generations unchanged.

A changed input or observation definition invalidates existing observations and
revision tokens. The migration advances both counters and clears the recorded
observation generations. Its signature includes the selected field definitions.
It does not erase the stored observation fields. New observations must confirm
them before they can be presented as current.

For versioned entities, generated `Migrate` commits schema changes, revision
triggers, and invalidation in one transaction. A later migration error rolls all
of them back. External migration tools must preserve the same transaction
boundary for related schema changes. The migration takes an exclusive table lock
and can update all retained rows when the contract changes. Plan an explicit
upgrade for production data volumes. Do not run an older migration after an
upgrade. Use a migration role; the application role must not own the schema or
have DDL, TRUNCATE, replication, or trigger-bypass privileges.

Startup verifies the function contract and metadata columns. An old or incomplete
contract fails startup. Database administrators remain trusted. The checks do
not protect against an administrator changing data or schema after startup.

## Evidence and remaining work

Generated PostgreSQL tests cover independent groups, unchanged-value confirmation,
raw input changes, JSON equality, binary and timestamp values, denied field sets,
current-state lists and predicates, transaction rollback, changed contracts,
repeated migration, and atomic rollback
of schema and observation metadata. Earlier revision and concurrency tests also
apply. The Gateway application test crosses REST, gRPC, event delivery, and
restart. The workload controller still checks the provider when its observation
is current, so generation equality does not disable drift repair.

A generation covers fields on this row. It does not track changes inside a
referenced resource, provider code changes, or external drift. Those changes still
require observation and repair. Durable cleanup completion, cross-process fencing,
condition history, observation timestamps, and production capacity remain open.
