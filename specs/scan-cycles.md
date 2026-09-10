# Durable scan-cycle outcomes

`ScanCycle` retains action failure evidence across bounded passes and process
replacement. It uses `ScanFrom`, the observation time reserve, and existing
versioned checkpoint callbacks. The application supplies the source version,
action, access checks, and the policy for continuing after an action error.

`Complete` means that the source ended. Success also requires `Failed=false`
and a successful conditional save. A failed item can permit later independent
items. Its failure remains in the stored cycle even if a later pass has no new
errors. The completed failed cycle returns `ErrCycleFailed`. The next full
cycle starts without that failure and must perform the work again.

A changed source version starts at the beginning. The save must check that the
source version is still current. Alternatively, a dependency write can reset
the checkpoint and advance its version in the same transaction. The retained
checkpoint version rejects an old save after that reset. A cursor version is
not a provider lease and cannot undo an external action.

`EncodeCycle` and `DecodeCycle` define a canonical, versioned record for opaque
checkpoint storage. It contains the source version, cursor, failure flag, and
completion flag. Source versions contain 1–128 UTF-8 bytes. Cursors contain at
most 512 UTF-8 bytes. NUL is rejected. The encoded record is at most 1,024 bytes.
No provider error text is stored. Unknown or malformed formats fail before work.
`ValidateCycleTransition` rejects loss of earlier failure evidence within the
same unfinished cycle. API adapters can use this check before a conditional save.
The runtime checks each whole page for persistence bounds before its effects.

A work timeout can save the completed prefix and failure state with the reserved
commit time. Parent cancellation prevents the save. A provider callback that
returns nil after its deadline does not advance the cursor. A failed save can
repeat work; actions must tolerate repeats. A returned state does not prove that
its save committed. Callers must inspect the returned error and stored evidence.

Generated tests cover failure across process replacement, the next clean cycle,
source changes, save conflicts, malformed records, unpersistable pages, deadline
handling, and parent cancellation. The Hypershell controller regression first
failed because a resumed final pass lost a previous user's provider failure.
The application now uses the common cycle runtime and PostgreSQL checkpoint
storage. Grant writes reset cycle evidence in their existing grant/event
transaction. Desired Gateway generation changes also invalidate cycle input.

These records describe a scan of the chosen inputs. They do not establish
provider liveness, a maximum observation age, a cross-page database snapshot,
or exclusive ownership. Hypershell uses these records for a scoped
`GrantsSynchronized` [condition](resource-conditions.md). Grant changes reset the
cycle and invalidate the condition atomically. Desired generation, resource
revision, and checkpoint version protect its commit. This condition covers
stored Gateway grant references; it is not general provider readiness.
`ClientReady` remains limited to client configuration.
