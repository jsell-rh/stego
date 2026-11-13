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

## The check against the base

`stego compat --base <ref>` checks the declaration against the declaration at
the base ref. The check takes the declaration at the base ref as the version
that runs now. A release that is not deployed before the next one is the
operator's risk. See [stego compat](compat.md).
