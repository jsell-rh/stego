PostgreSQL adapter 3.11.0 supplies `CleanupSummaryReader.ReadCleanupSummary`.
The caller selects an entity, one declared cleanup owner, a target when that
owner uses target history, and an optional exact text scope. The caller must
authorize the whole selected scope before reading. This method does not infer
permission from a resource name, status, or prior event.

The query returns the pending deleted-resource count, the oldest original
deletion time, and the database statement time. The oldest time is absent exactly
when the count is zero. It returns no resource IDs or payloads. One aggregate
statement computes count and minimum from the same database snapshot. Inside a
transaction, normal transaction snapshot rules still apply. The statement time
does not turn an older transaction snapshot into current state.

A target owner requires one nonempty target from the caller. Only rows whose
retained history includes that target can contribute to its count. Other owners
require no target. A completed owner or target is excluded. A reopened cleanup
uses the original deletion time, so a new failure cannot reset its apparent age.
Live rows are excluded even when they retain false cleanup observations.

The optional scope selects one declared string, enum, reference, or ID field.
Observation fields are not permitted as scope fields. Both scope arguments must
be present or absent. Values and targets are bound parameters with a 256-byte
limit, valid UTF-8, and no NUL. Exact text comparison remains case-sensitive under
a case-insensitive database collation. Unknown entities, owners, fields, invalid
target selection, and invalid scope pairs fail before SQL work. The existing
ten-second storage request limit also applies to this query.

This change adds no schema migration or stored counter. Counts reflect retained
rows and can require a table scan. They are sampled independently from resource
recovery; they are not added to each cursor page or provider action. The query
is a diagnostic read, not proof that external effects are absent or permission
to purge a row.

Controller component 1.10.0 adds `KeyedOptions.Cleanup`. With a metrics collector
enabled, the runtime calls this source on a separate worker. It applies the
configured action timeout, waits the resync interval after each read, and joins
the worker on shutdown. A blocked recovery scan cannot block this sampler.
With metrics disabled, the source is not called.

The generated metrics expose whether a source is enabled, whether its last read
succeeded, the pending count, the oldest deletion timestamp, the local time of
the last successful read, and fixed success/failure counters. A failed read
retains the last values but marks them unavailable. Shutdown also marks them
unavailable. A malformed sample or a terminal access error stops the controller;
other read failures are reported and sampled again. Error strings are not stored
in the collector. The application must still apply its log policy to notices.

Use the oldest timestamp to calculate age only when the pending count is positive
and the sample is available. Check the last-success time as well. Database,
application, and monitoring clocks must be synchronized for wall-clock age.
These gauges describe one authorized owner/target/provider scope. They do not
identify individual resources, cover every cleanup owner automatically, or
replace durable conditions and access-controlled diagnostics.

Generated PostgreSQL tests cover separate owners, retained targets, reopening,
empty summaries, exact scopes, quoted values, case-insensitive collation,
transaction-local reads, rollback, unchanged revisions, and invalid requests.
The sampler tests cover refresh during a blocked scan, failure and recovery,
invalid samples, shutdown, and disabled collection.

A PostgreSQL 18.6 benchmark used one owner with all rows deleted and pending.
On an Intel Core Ultra 9 185H with Go 1.26.8, three runs measured 0.279–0.362 ms
for 1,000 rows and 1.986–2.108 ms for 10,000 rows. Each call made 112 allocations
and used about 8.7–9.1 KiB. The benchmark includes the storage query and decoding;
it excludes service transaction setup, gRPC, authentication, other workloads,
and a production retention
volume. Query limits and production capacity still require deployment evidence.

The full compiler race suite passed with PostgreSQL required. Static checks
also passed. The first full run found an old registry-version assertion; that
assertion now matches the new adapter version.
