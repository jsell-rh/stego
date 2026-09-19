# Durable scan-cycle outcomes

`ScanCycle` retains action failure evidence across bounded passes and process
replacement. It uses `ScanFrom`, the observation time reserve, and existing
versioned checkpoint callbacks. The application supplies the source version,
action, access checks, and the policy for continuing after an action error.

`Complete` means that the source ended or reported an explicit failed window. Success also requires `Failed=false`
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

## Failed source windows

Controller 1.21.0 adds `ErrScanWindowLimit`. Only a source can request this
boundary. The current cycle is saved as complete and failed, and returns an
error. The next call starts a full scan. An emitter cannot use this error to
end a window. Parent cancellation prevents saving, and a save conflict remains
an error. This permits recovery after deletions change an offset-based list;
it does not make the list a snapshot.

All six STEGO jobs passed for `af67e7b` in
[run 35106013382](https://github.com/jsell-rh/stego/actions/runs/35106013382).
The generated controller tests passed with the race detector. Real Keycloak
passed in 54.43 seconds. Resource-state SQL, provisioning, dependency checks,
and both examples passed. The full log has SHA-256
`5fe74a77cc5091ef4ee92bf5dca58e9d62a2d23e7de10abba672c2f7c406d4fd`.
Raw evidence is in `provider-window-common-result` and
`provider-window-full-ci.log` under the persistent Gateway cleanup run directory.
The complete Hypershell checks remain separate.

## Action time reserve

Controller 1.22.0 adds `ScanCycleWithOptions` with `CycleOptions.ActionTimeout`.
The timeout must be at least one millisecond and less than the work timeout.
Before each action, the runtime checks the remaining work time. If a full action
budget is not available, it stops the pass and saves the successful prefix.
This planned stop does not add a failure flag. The next call resumes after that
prefix. Each admitted action receives a context with its own timeout.

An action error or a late nil return still adds failure evidence. A later pause
cannot erase that evidence. Parent cancellation still prevents the save, and a
save conflict still returns an error. The original `ScanCycle` API and checkpoint
format remain unchanged. This API does not increase any time limit, add workers,
or set application policy. Callbacks must honor cancellation and tolerate repeat
work.

The change follows Hypershell capacity run `35461469012`. Its diagnostic record
showed 100 closed account rows by 43.1161 seconds, but the scope was not sealed
at 120.0213 seconds. Two completed scans retained failure flags. Provider
inventory never started. This record does not prove final provider absence or
background preservation. The 30-second target failed. The next consumer test
must check the action reserve before any scheduler or timeout change.

Generated tests use a controlled clock to check multi-pass completion, retained
provider failures, save conflicts, invalid limits, and parent cancellation.
Both generated variants, with and without telemetry, passed those checks.
Source `58a3bcc` passed all six branch and main compiler jobs, including 34
packages with race checks. The signed main build produced the same bytes as
the branch build. The common installer verified the source and both signatures
before and after immutable release publication. See the
[qualification record](scan-action-budget-evidence.json).

Hypershell's six-account test failed with the old compiler at `eed9066`.
It was the only failed journal test; the other 32 passed with no skip.
The fixture uses PostgreSQL and a bounded provider delay. It does not prove
real-provider capacity. Consumer adoption and improved cleanup timing remain
unproved. The 30-second target is unchanged.

## Parallel actions by resource key

Controller 1.23.0 adds the opt-in `ScanCycleParallel` API. Its typed options
require 1–64 workers, an action time limit, and a resource key function.
`MaxParallelCycleWorkers` exposes the worker ceiling for startup checks. The
runtime checks every key before page effects. Keys contain 1–512 valid UTF-8
bytes, without NUL. The runtime does not store or log keys.

Actions for equal keys run in source order, one at a time, within the call.
Different keys can run at the same time. A page can contain the same resource
under different source cursors, so cursor uniqueness is not a concurrency key.
The caller must supply the resource identity. This is not a distributed lock.

The runtime checks the full action time reserve before dispatch and before the
worker starts an action. A planned stop waits for admitted actions. A terminal
error stops admission and cancels the page. All workers stop before checkpoint
storage. External effects can finish after a callback returns; the key queue
does not fence those effects. Continuation policy calls are serial. The policy must not depend on
action completion order. Effects beyond a stopped prefix can repeat after
restart, so all effects must remain safe to repeat.

Only the contiguous accepted prefix advances. Permitted action errors still
retain the cycle failure flag. Cancellation after a peer failure cannot accept
an interrupted action. Parent cancellation prevents saving. Conditional saves,
source windows, and the checkpoint format retain their existing rules. The two
existing sequential cycle APIs keep their behavior.

This candidate follows Hypershell run `35463991341`. Complete account cleanup
took 93.2722 seconds; the 30-second target failed. All selected provider objects
were absent and background state was preserved. Saved passes had clean failure
flags, but handled about 18–21 account rows before a wait of about 12 seconds.
The bounded parallel candidate and its generated tests still require CI.
No consumer timing improvement is claimed.


## Parallel runtime qualification

Compiler `d3ccd11` supplies controller component `1.23.0`. All six branch and
main compiler jobs passed, including 34 race-tested packages. The focused
checks passed all 13 parallel cases in generated runtimes with and without
telemetry. The cases cover key ordering, budget pauses, retained failures,
cancellation, callback joins, safe prefix recovery, and save conflicts.

Two isolated main builds produced identical bytes. The common installer
verified both signatures, source identity, and workflow identity. The immutable
release was downloaded and checked through the same installer. See the
[complete qualification record](scan-parallel-evidence.json).

This proves the common runtime at the recorded source. Hypershell adoption and
its real-provider capacity result remain separate. No timing improvement,
distributed fencing, or exactly-once effect is claimed.
