The generated Kubernetes client provides `WatchSet` for a changing set of
namespaced collections. The application supplies a complete assignment and a
verified identity for each namespace. STEGO owns watch startup, cancellation,
replacement, baseline serialization, and aggregate object limits.

`Replace` validates the whole assignment before it changes a watch. Paths must
refer to namespaced collections. Cluster-wide paths, object paths, duplicate
paths, missing identities, and excess assignments are errors. The caller must
use a dedicated client. The watch count cannot exceed that client's configured
`WatchLimit`, which defaults to 16 and can be one through 128.

The allocator's `NamespaceUID` reads a live namespace and verifies its declared
owner and installation labels. A missing or deleting namespace is pending.
This read does not establish Pod access or prove that all role bindings exist.
The application must still select namespaces from current authorized state.

`Run` calls its consumer serially. Each change carries the collection and its
assignment identity. A changed identity stops and joins the old watch before
the replacement starts. Removed scopes produce `REMOVED`. `RESET` invalidates
one scope. The consumer must discard its old objects for either event. Only
`REPLACE` supplies a complete baseline. A missing scope is not a zero count.

Provider failure invalidates the affected scope and supplies an error with
`RESET`. Other watches continue. A later `Replace` can retry that scope after a
new assignment check. The caller supplies this schedule; `WatchSet` has no
polling timer. Consumer errors and contract failures stop the set. Cancellation
joins all its watches. Callbacks must return promptly and honor cancellation.

Only one complete list can be in flight at a time. The default aggregate limits
are 10,000 objects and 64 MiB of encoded object data. Callers can lower them.
The manager rejects objects from a different namespace and refuses an event
before its scope has a complete baseline. Limits cover all retained scopes,
including object replacements and removals. They are not a process memory
limit: decoded objects and each stream's bounded input frame need more memory.

Generated tests use TLS list/watch endpoints for additions, removals, identity
replacement, denied reads, invalid assignments, aggregate limits, and foreign
objects. They also check baseline serialization, consumer failure, reuse after
shutdown, and joining removed workers. The tests run with and without OTEL.
The allocation tests check pending, foreign, replaced, and deleting namespaces.
Application and live permission checks remain necessary for each consumer.
