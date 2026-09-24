# Schema migrations

## Purpose

This specification defines how schema changes reach a database that STEGO
generates. It gives operators one answer to two questions: which schema
changes has this database had, and is it safe to apply the next one?

## Contract

- The database keeps a migration ledger in `stego_schema.migrations`. Each
  row records the migration version, the digest of the migration body, and
  the time of application. The runner owns all writes to the ledger;
  application code never edits it.
- A migration is a named body. SQL migrations carry the SQL text; function
  migrations carry the registered name. The digest covers the body, so an
  edit after application is detected.
- Migration names must sort in apply order. Use zero-padded number
  prefixes.
- On a fresh database, all registered migrations apply inside the bootstrap
  transaction, and the ledger rows commit with it. If the bootstrap fails,
  no ledger row survives.
- After bootstrap, each pending migration applies in its own transaction,
  and its ledger row commits in the same transaction. A migration and its
  record commit together or not at all.
- The applied history must match the registered history exactly. The
  runner rejects these cases:
  - a version that was already applied (re-application),
  - an applied version that the code no longer registers (foreign history),
  - a missing version between applied ones (gap),
  - a digest that no longer matches the registered body (edit after
    application),
  - a version that sorts before the applied history (rolled-back
    database).
- Databases without schema generation keep the plain ordered runner and
  carry no ledger. The ledger is part of the schema generation contract.
- `Migrate` runs the registered list. `ApplyMigration` applies one SQL
  migration by body, for databases whose migrations run outside the
  application. Both enforce the same continuity rules.
- `AppliedMigrations` reads the ledger. It is read-only and fails closed
  when the ledger is missing.

## Security

- The ledger lives in `stego_schema` with no public grants. The
  application role receives SELECT and INSERT only: it can record an
  applied migration, but it cannot edit or delete the history.
- A tampered ledger fails closed: the next migration run refuses to
  continue rather than trust a history that does not match the code.
- The bootstrap shape check verifies the ledger table structure before
  the database serves requests.

## Not proved

- Function migration bodies are digested by registered name only. An edit
  to a function body after application is not detected; SQL migration
  bodies are covered.
