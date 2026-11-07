# Lifecycle records

This folder holds the lifecycle records of the registry's component digests and runtime library versions: one
OSV advisory for each supersession or withdrawal, named `<advisory id>.json`. Write them with
`stego lifecycle supersede` and `stego lifecycle withdraw`, and commit them.

STEGO reads this folder at the registry's current head, not at the snapshot a service pins, so a service on an old
snapshot still learns that a pin is superseded or withdrawn. A registry without this folder cannot be checked, and
validate, plan and apply then refuse to run. Keep the folder even when it holds no advisory.
