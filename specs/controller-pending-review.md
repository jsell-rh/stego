# Pending controller work

This source review applies to compiler `ee348b8` and Hypershell test source
`80b0e00`. It identifies a runtime distinction that needs review. It does not
identify the cause of the observed 53.95-second Gateway cleanup time. The
current main workflow and request timing are recorded below.

## Current behavior

The generated keyed controller accepts an action that returns one error value.
In `internal/generator/controller/keyed.go.tmpl`, every nonterminal action error
uses the same capped retry schedule. The action receives a failure outcome
unless the error is a timeout or cancellation. A later invalidation marks a
queued key as changed but does not shorten its failed-action delay. The queue
retains this delay across watch reconnection.

`internal/generator/controller/metrics.go.tmpl` exposes four action outcomes:
`success`, `failure`, `timeout`, and `canceled`. The common telemetry wrapper
passes the action error to the trace and log completion code. There is no
separate result for expected incomplete work. These controller templates are
unchanged between the pinned compiler and the current documentation revision.

Hypershell supplies two examples of expected incomplete work. Its namespace
allocation controller returns `ErrPending` while an allocated namespace is
still present or a required SQL cleanup record is incomplete. Its workload
provider returns a different `ErrPending` while Gateway or Sandbox namespace
removal is incomplete. The workload controller preserves that result through
`RunObservation`. Both controllers use a one-second minimum and ten-second
maximum retry delay. Neither terminal policy treats these pending results as
terminal errors.

Thus, expected incomplete work uses failure telemetry and the error retry
schedule in these paths. This follows from the source. The effect on the live
cleanup duration has not been measured. Do not classify the total duration as
provider latency or change the retry policy from this source review alone.

## Required review after the phase test

The phase test records namespace removal, durable cleanup flags, finalization,
and the later proof checks. Require all 14 observations and the complete
workflow result before selecting a timing change.

If a distinct pending result is added, STEGO must own the result contract,
queue scheduling, and telemetry. The application must still decide whether its
current authorized state is incomplete. The common runtime must not contain
Gateway names, namespace order, SQL cleanup dependencies, or application roles.

The design and tests must cover these conditions:

- Pending work has a bounded recheck schedule. It does not occupy a worker while
  waiting or create an unbounded set of timers or goroutines.
- Pending work remains inside the existing queue capacity and preserves one
  active action for each key. Other keys can still make progress.
- Real errors, terminal errors, cancellation, and deadline expiry cannot be
  hidden by a pending result. A commit error after incomplete provider work
  remains an error.
- Error retry delays still survive watch invalidations and reconnects. A faster
  check for expected progress must not remove this protection from real errors.
- A pending action does not imply successful provider work, a successful
  observation commit, or resource readiness. Existing version, ownership, and
  durable cleanup checks remain required.
- Logs, metrics, and traces agree on the pending outcome. Labels remain fixed
  and contain no keys, resource IDs, addresses, credentials, or provider errors.
- Existing action contracts retain their documented behavior. Any new contract
  is explicit and rejects invalid delays before work is scheduled.

Validate the common behavior with an independent generated service as well as
the Hypershell workflow. Pending scheduling does not supply distributed fencing,
rollback detection, or proof of production capacity.

## Completed phase evidence

Hypershell run 35481347942 completed all eleven required tests at source 80b0e00.
Its source, generated output, compiler records, screenshots, account records,
and cleanup were checked. The fourteen phase records show finalization at
29.73 seconds, followed by a Gateway state namespace still present at 51.69
seconds. Complete cleanup proof took 54.34 seconds. The final account proof
used about 1.42 seconds. See the
[application review](https://github.com/jsell-rh/hypershell-stego/blob/main/acceptance/cleanup-phase-review.md).

This evidence requires a correctness fix before a scheduling change. The
application had no cleanup owner for namespace allocation. Its candidate now
uses the existing generated targeted cleanup records and observation runtime.
The namespace and SQL removal order remains application policy. STEGO does not
need Gateway-specific queue or storage behavior for that fix.

The account cleanup flag changed from complete back to pending thirteen times.
In the reviewed application source, RecoverGatewayCleanup sets its completion
value from the current bounded scan and inventory result, then always supplies
that value to RecordCleanup. A bounded scan that has not reached its end can
therefore supply false during a repeated verification. The source explicitly
requires repeated checks for late provider effects. This is a possible source
of the observed changes, not proof of their exact cause or latency effect.

A later change must distinguish incomplete verification from new evidence of
unfinished work. It must preserve checks for changed generations, new journal
members, provider failures, late effects, and failed observation commits. Do
not keep a completion record merely to reduce event count. Do not stop retained
recovery merely because the public resource is finalized. Any common change
still needs independent generated-service checks and the application workflow.

The 30-second whole-Gateway target, distributed fencing, and the other open
enterprise requirements remain unproved. At that checkpoint, the candidate allocation fix had not
yet passed its new live pause-and-recovery check.

## Current main workflow and request timing

Hypershell main `e9b9bf9` passed all 11 browser workflow tests in
[run 35486475436](https://github.com/jsell-rh/hypershell-stego/actions/runs/35486475436).
Independent checks verified 1,570 source files, 421 generation hashes, the
actual signed compiler bytes, and the bounded test Job. The test retained the
deleting Gateway while its allocator was stopped. It completed deletion only
after the allocator resumed and removed the state namespaces.

Normal cleanup of one Gateway with 100 accounts took 58.2276 seconds. The
30-second target remains open. The account observation returned to pending
14 times. The separate account rescan fix is not part of this source.

A read-only observer retained safe fields from the allocator's existing
structured logs. In the cleanup time window, it recorded 229 HTTP requests.
Their maximum duration was 0.0581 seconds. Eight reconciliation results reported
failure at approximately 0.05, 1.08, 3.13, 7.15, 15.26, 25.32, 35.47, and 45.59
seconds after the observed DELETE response. These intervals follow the
configured retry delay: one second, then two, four, eight, and ten seconds.

The observer used a bounded log tail and can omit records. Its time window
compares clocks in different Pods. The records contain no resource key, so
other work can occur in the window. These observations support investigation
of the retry policy. They do not prove the cause of every cleanup delay or
predict the result of a runtime change. See the
[saved measurement summary](controller-pending-evidence.json).

The source distinction described above remains unchanged. This measured
window now supports the common runtime review. It does not replace the safety
requirements or the independent generated-service checks. The application
must retain namespace dependency order and durable cleanup ownership checks.
Only verified absence can complete namespace cleanup.

No runtime or retry policy changed in this review. The current API test and
full hosted suites remain separate from this completed browser result. Live
Kata isolation, restore, fencing, and production capacity remain open.
