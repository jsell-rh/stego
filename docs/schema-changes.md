# Schema changes

After the first release of a service, its tables can have rows. STEGO cannot
count rows, so it takes each table of the base as a table with rows. A table
with rows changes only by a declared schema change, one phase for each
release.

## Declare a schema change

A schema change has an ID, a kind and the entity and the field it changes.
The kinds are `add`, `rename`, `retype`, `require`, `drop` and `unique`. The
schema history puts the steps of the changes in order. A step moves one change
to `expand` or to `contract`.

```yaml
schema_changes:
  - {id: note-title-rename, kind: rename, entity: Note, field: heading, from: title}
schema_history:
  - {change: note-title-rename, phase: expand}
```

The release with the expand step adds. The release after it, with the contract
step, removes what only the old release needed. The declaration keeps every
schema change, also a finished one.

## Add steps

`stego apply` writes the add steps for you. Before it plans, it puts one
schema change of kind `add`, with its expand step, at the end of the schema
history for each new entity, and for each new field that is optional or has a
default on an entity of the base. Their IDs are `add-<entity>-<field>` and
`add-<entity>` in lower kebab case. Apply names each step that it writes;
commit them with the record. `stego plan` lists the steps that apply writes
and saves no plan until they are in the declaration. Apply writes no other
kind of step: declare a rename, a retype, a require, a drop or a unique
yourself.

Plan and apply check the declaration against the base before they plan: the
ref that `--base` names, or the default branch of the repository. The default
branch is the branch of `origin` that `refs/remotes/origin/HEAD` names, else a
local `main`, else `master`. When the check refuses the declaration, plan and
apply write nothing. When the service folder is in no Git repository, there is
no check, and plan and apply say so. A repository with no such branch, or one
that git cannot read, stops plan and apply: name the base with `--base`.

## The migrations of the steps

`001_initial` holds the tables of the first release: the declaration with
every schema step undone. Each step is one more migration, after the
migrations of the components, in the order of the schema history. Its name
is `step_<position>_<change>_<phase>`, such as
`step_0001_add-note-body_expand`. The migration of an add creates the table
of the new entity, with the row-level security of an owned table in a pooled
service, or adds the column of the new field with its checks, its foreign
key and its indexes. It changes nothing that is there. `stego plan` lists
each new step with its change, kind, phase, migration, trigger and backfill.
A new database and a database that each release migrated in turn get the
same schema.

An add of a field that is required and has no default is refused, because
the rows that the table holds cannot take it. Make the field optional or
give it a default. An entity that refers to an entity that a later step adds
is refused too: put the step that adds the other entity first.

The marker of the database holds the digest of the stored shape of the
first release, from postgres-adapter 6.2.0. A change of a collection or a
permission does not change it. The first migration job of postgres-adapter
6.2.0 or later on a database that an earlier release made writes the new
digest into the marker, once. A pod of the earlier release that restarts
after that job fails its marker check: during that rolling deploy, do not
restart the pods of the earlier release. A change of the indexes of a collection over
an entity of the base is refused, because no schema step builds such an
index yet. See [stego compat](compat.md).

The release before a step must run postgres-adapter 6.1.0 or later, because
an earlier ledger refuses the row of a step that it does not know. The check
against the base refuses a new step on such a base and names the version to
deploy first.

## Restarts during a rolling deploy

A pod of the release before a step restarts on the database after the
step's migration. Its schema check accepts what a later step adds, each only
in the exact form that the step gives it, on the table of a declared entity:

- the foreign key of an add of a reference field: on the new nullable column,
  to the primary key of the table of an entity that it knows, with the tenant
  column first when the referenced entity is owned, with no action,
  validated, not deferrable, and named `<table>_<column>_fkey`
  (ST-SUP-RP15-LC03). A foreign key on one column refers only to a shared
  entity, whose rows each tenant may read;
- the default of the new column of an add: a constant of a built-in type,
  and no function call (ST-GEN-Q15);
- the sync trigger of the expand step of a rename, a retype or a require:
  a BEFORE INSERT OR UPDATE row trigger whose function is the fixed
  template of this release, in the schema `stego_steps`, for one column
  that the release before knows and one new column (ST-SUP-RP15-LC02).

A column that the manifest does not list, on any table of the manifest,
must be of a built-in base type of `pg_catalog`, such as `text`, `uuid` or
`integer`: a domain, an enum, a range or a composite type can run a function
on each insert, so it fails the check. Every other change to such a table still fails the check. A release before
this rule refuses these forms: during the first rolling deploy of a step
after it, do not restart the pods of a release that does not have it.

## The check against the base

`stego compat --base <ref>` checks the declaration against the declaration at
the base ref. The check takes the declaration at the base ref as the version
that runs now. A release that is not deployed before the next one is the
operator's risk. See [stego compat](compat.md).
