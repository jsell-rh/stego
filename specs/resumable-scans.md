Controller component 1.8.0 adds `ScanFrom`. It reads bounded cursor pages from
an application source and returns `ScanProgress{After, Complete}`. The cursor
identifies the last item whose emitter returned success. The source must accept
resumption after any item, including an item in the middle of a former page.

Each call reads at most `MaxPages`. Reaching that budget returns incomplete
progress without an error. The caller must schedule another pass. A source or
emitter error returns the completed prefix. If an emitter succeeds and then the
context is canceled, its cursor remains completed. A failed emitter does not
advance the cursor. A whole page is validated before any of its effects start.
The runtime bounds page size, page count, cursor length, and page-read time.
It does not compare opaque cursors using Go string order.

The existing `Scan` function starts from the beginning. It still reports a
contract error if its page budget cannot cover the source. Its callers do not
silently change to partial scans.

The caller owns cursor persistence, resource scope, payload validation, retry
policy, and provider actions. `ScanFrom` starts no background work and supplies
no durable acknowledgment. Cursor progress is not a state snapshot. Actions
must read current state, tolerate repeats, and honor cancellation. Later full
scans must recover changes before a saved cursor. A source that cannot return a
stable order must not use this cursor contract.

The Hypershell identity controller uses grant IDs from the common PostgreSQL
cursor adapter. Its previous page-number scan stopped after 10,000 references.
A regression with 10,100 references failed at that limit. The new controller
retains progress between bounded passes and starts another full scan after the
end. Hypershell supplies the private RPC, grant-to-user mapping, access policy,
and provider action. STEGO supplies page validation and scan progress.

This change does not resolve durable controller progress, bounded storage for
incomplete resource cursors, or cross-process fencing. Hypershell still keeps
its cursor map in process memory. A process restart starts a new full scan.
A steady source and responsive dependencies remain conditions for progress.

The full STEGO race suite and `go vet ./...` passed. Generated tests cover
partial-page cancellation, failed-item retry, page-budget continuation,
malformed pages before effects, and non-lexical cursor order. Existing watch,
sweep, and full-scan tests also passed.
