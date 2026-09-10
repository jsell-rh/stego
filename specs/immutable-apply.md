CLI component 1.4.0 adds immutable resource support to apply.
`ApplyResource.ImmutableIdentity` declares one through eight fields used for
exact lookup. Each identity field must be a required, non-null string in
`CreateFields`. Its value must be nonblank and at most 256 bytes. Immutable
resources must have no `PatchFields`.

The manifest still has `kind`, optional `apiVersion`, `metadata`, and `spec`.
For an immutable resource, `metadata.name` is an optional output label. It is
not sent to the API and is not used for lookup. Identity values belong in
`spec`. `metadata.id` can select an existing resource, but its returned fields
must still match the requested spec. A selected ID that is missing does not
cause creation with a different ID.

The API must supply resource objects with `id` and the requested fields, and
list objects with `items` and `total`. List lookup uses escaped string equality
conditions joined by `and`, with page 1 and size 2. The runtime checks the total,
item count, exact identity, and every requested field. It rejects ambiguity and
contradictory responses. Numbers retain their exact values during comparison.
The API must enforce identity uniqueness for concurrent creation. Hypershell
already has a unique index for live Gateway/user/role bindings.

All lookup and field checks occur before resource writes. If no resource exists,
apply sends POST. If an exact match exists, it reports `unchanged` and sends no
write. A mismatched existing resource causes an error. The runtime never sends
PATCH or DELETE for an immutable resource. Extra fields omitted from the spec
are not managed by this apply operation. A no-op confirms readable state; it
does not establish write permission.

After a POST conflict (HTTP 409), the runtime performs one fresh lookup. It
reports `unchanged` only if that lookup finds one exact match. It does not retry
a write. Missing, denied, ambiguous, or mismatched results remain failures.
An uncertain response after a write retains the existing `unknown` outcome and
retrieval guidance. A batch is not a transaction across resources. Earlier
successful writes remain if a later operation fails.

Named mutable resources keep their existing name lookup and PATCH behavior.
Generated TLS tests cover ordinary and nameless manifests, repeated apply,
quoted identity values, exact large integers, conflicts, ambiguous lists,
wrong identities, wrong write responses, and denied reads. Configuration tests
reject optional, nullable, non-string, duplicate, and unknown identity fields.
The Hypershell mapping supplies four fields and no new runtime implementation.

The full compiler race suite and static checks passed. The final generated CLI
race fixture also checks boolean values, ordered string lists, null values,
and integers above the exact floating-point range. A comparison benchmark used
four 64-byte string fields on an Intel Core Ultra 9 185H with Go 1.26.8.
Three runs measured 8.766–9.519 microseconds, 12,736 bytes, and 64 allocations
per comparison. This includes field validation and exact comparison. It excludes
input loading, HTTP requests, authentication, and database work. It is not an
application capacity measurement.
