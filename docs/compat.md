# stego compat

`stego compat` tells you, before a merge, whether a change to `service.yaml`
can run beside the version of the service that runs now, on a database that
has rows.

```sh
stego compat --base main
stego compat --base main --json
```

## The version that runs now

STEGO cannot see production. The check takes the declaration at the base ref
as the version that runs now. In the change gate, the base ref is the base of
the pull request. The check reads `service.yaml` and the record
`.stego/state.yaml` at that ref from the repository of the service.

A release that is not deployed before the next one is the operator's risk.
If you skip a release in production, the check did not compare your change
with the version that really runs.

## What the check refuses

When the base ref has no record, no release of the service has run, and the
check accepts any stored shape.

When the base ref has a record, the check refuses:

- A declaration at the base ref that is not the declaration that its record
  names. Run `stego apply` on the base and commit the record.
- A change to the stored shape of an entity of the base that no schema step
  declares: a new, removed, renamed or retyped field, a new required field, a
  new entity, a removed entity, or a new invariant of kind `unique`.
- A schema history that does not start with the steps of the base, in the
  same order and unchanged.
- A schema change of the base that the head removes or changes. Only a retype
  at expand may get a fill value.
- A schema change that gets its expand step and its contract step in one
  release.
- A change to `001_initial` or to the schema marker that no schema step
  declares. The check generates both for the base and for the head, with
  every schema step undone, and compares them table by table. This finds a
  new order of the fields, a field marked `pii` or `secret` that leaves a
  search index, and a change to the indexes that the collections give an
  entity of the base: a collection that starts or stops listing it, a new
  scope or implicit field that has no search index, or a changed upsert key.
  No kind of schema change builds such an index yet. A collection change
  that gives no index, and a change of permissions, are accepted. When
  generation does not carry out a kind of schema change of the declaration
  yet, such as a rename, the check compares the indexes that the
  collections describe.
- A new schema step when the base runs postgres-adapter before 6.1.0. Deploy
  a release with postgres-adapter 6.1.0 or later first.

Each fault names the entity, the field, the rule and the schema change and
phase to declare. See [schema changes](schema-changes.md).

## Exit status

- 0: no fault.
- 1: one or more faults.
- 2: a usage error, or a base ref that the check cannot read.
