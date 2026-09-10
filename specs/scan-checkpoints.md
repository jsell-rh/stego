# Durable scan progress

A controller can reach its page or work limit before it reads all references.
`ScanCheckpointed` saves the completed prefix through application callbacks.
The generated PostgreSQL adapter supplies `CheckpointStore`. Hypershell uses
these two common contracts for its Gateway identity grant scan.

The application supplies a source, an action, a fixed scope, and access checks.
STEGO owns page validation, work and commit deadlines, and conditional cursor
storage. No Hypershell entity or provider rule occurs in the implementation.

## Execution

The runtime loads the checkpoint before work. Version zero means that no row
exists. An empty cursor starts a full scan. It runs at most `MaxPages` pages
and reserves `CommitTimeout` within the parent deadline. It saves the cursor
of the last successful action if that cursor changed. Source completion resets
the cursor to empty. A stable full scan that starts empty needs no write.

A failed action keeps its item eligible. An application that records an action
error and returns success must retry that item in a later full scan. Current
state must be read before each provider action. A saved cursor is not a snapshot,
an event acknowledgment, or proof that provider state has converged.

A work timeout permits a save with the remaining commit budget. Parent
cancellation prevents the save. Callbacks run synchronously and must obey their
contexts. A crash, a cancelled parent, or a failed save can repeat work since
the previous checkpoint. Provider actions must be safe to repeat. Neither
contract retries a stale save with a newer version.

## Storage

The key is an exact, case-sensitive entity, resource ID, and scope. Entity names
must occur in the declaration. IDs contain 1–256 bytes; scopes contain 1–128
bytes. Cursors contain at most 1,024 bytes. Strings must be valid UTF-8 without
NUL. These limits apply before a query. The application must limit scope names;
use one fixed name for each scan purpose. Row count is bounded by the number of
resource and scope pairs that the application permits, not a global store cap.

Saves require a generated store transaction. A first save inserts version one.
Later saves compare the supplied version and increment it. A conflict or
invalid save also prevents commit if the callback ignores the error. Resetting
a cursor retains the row and increments its version. Do not delete that row
while an older writer can still run. History retirement needs a separate rule.
The cursor has its own version; it does not change a public resource revision.

Apply `000006_scan_checkpoints.sql` before new code starts when migrations run
externally. Startup verifies required column types, exact key columns, the
immediate valid primary key, required byte and version bounds, and absence of
row-level security on this internal
table. Application roles must not have schema change permission. Runtime checks
also validate stored values. Checkpoint storage does not authorize access, lock
the resource, or establish exclusive ownership of provider actions.

## Evidence and limits

Generated runtime tests cover a work timeout with a successful cursor save,
resumption at the failed item, parent cancellation, and a stale save error.
Generated PostgreSQL tests cover store replacement, exact keys, completed-scan
version retention, stale writes, rollback after an ignored failure, transaction
lifetime, invalid input, and missing schema constraints.

The Hypershell regression uses 10,100 references. Before this change, a new
controller repeated the first 10,000 references and could not reach the tail in
its next pass. Application tests also use a real API and PostgreSQL. The generated runtime
reads all 10,106 retained references across API restart. A separate test replaces
both the API and the Gateway identity controller after a work timeout. It
checks saved progress, current grants, and the unchanged public resource
revision. That test uses a recording provider; existing identity workflows
provide the real Keycloak evidence.

This contract does not provide distributed provider ownership. A cursor version
check prevents a stale bookmark write. It cannot undo a provider action that
ran before that check. Repeated full scans, current-state checks, and safe
provider actions remain required.
