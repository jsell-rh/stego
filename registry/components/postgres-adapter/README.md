# postgres-adapter

Version 5.0.0 removes the GORM store (ST-SUP-RC20-OC39). STEGO has one data
path: the generated commands and queries on the runtime library
(`registry/runtime`) carry all data access. This component does not emit a
store, an entity model, a session factory, a generic DAO or GORM
`AutoMigrate`.

For a service with entities, the component emits these files:

- `tables.go` and `migrations/001_initial.sql`: the table of each entity as
  SQL from the declaration, with the tenant column first in a pooled service,
  the key `id`, the declared fields with their checks, `created_time`,
  `updated_time`, `version` and, for a protected entity, `authz_token`.
- `migrate.go`: the migration ledger in `stego_schema.migrations`. `Migrate`
  applies each registered migration one time, in order, and records the
  digest of its body. A row that is not the registered history stops the
  migration with `ErrMigrationLedger`.
- `schema_generation.go`: the [schema generation marker](schema-generation.md).
- `database.go`: `OpenDatabase`, with verified TLS, the loopback test mode
  and the pool bounds of the `DATABASE_POOL_*` settings.
- `runtime_access.go`: `TenantConfig` gives the runtime library's connectors
  the configuration of `OpenDatabase`. `CheckLedger` lets the migration job
  refuse, before it writes, a database whose ledger is not the history of
  this release or that holds a table of the declaration with no ledger.
- `schema_manifest.go`: `SchemaManifest`, what the commands and queries need
  of the tables: the columns with their types and `NOT NULL`, the
  constraints, the primary key and unique indexes and, on an owned table,
  row-level security with its tenant policy. It comes from the same model as
  `001_initial`. At startup the service runtime reads the catalog as the
  runtime role and does not start on a database that does not hold it. The
  log names each missing or different object (ST-SUP-RC01-OC12).

For a service with no entity, the component emits `database.go` only.

## Settings

| Setting | Values | Use |
| --- | --- | --- |
| `schema_generation` | a lower-case name | Necessary for a service with entities. The name of the marker. |
| `migrations` | `startup` (default), `external` | `external` lets the migration job run the migrations. |
| `writer_lease` | `off`, or leave it out | The single-writer lease of the GORM store is removed. The value `single` is refused. |
| `connection_string` | text | The database connection. |

## Refusals

The component refuses these declarations with an error that names
ST-SUP-RC20-OC39:

- A service with entities that does not have the `command` and the `query`
  components. Add the two components, or use an archetype that has them.
- An entity with `generation_fields`, `observations`, `conditions`,
  `cleanup_owners` or `cleanup_targets`. Only the GORM store used them.
- `writer_lease: single`.

The `tsl-search` component is removed too. A list search of `rest-crud` uses
the equality filter of the query runtime (ST-SUP-RP11-LC20).
