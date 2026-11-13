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

## The check against the base

`stego compat --base <ref>` checks the declaration against the declaration at
the base ref. The check takes the declaration at the base ref as the version
that runs now. A release that is not deployed before the next one is the
operator's risk. See [stego compat](compat.md).
