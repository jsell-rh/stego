Frequent pushes must not prevent the full acceptance suite from finishing.
The prior concurrency rule canceled every active run when a newer push arrived.
On 2026-09-10, the seven most recent completed Hypershell runs were canceled.
For example, run 34496871848 passed its database, Gateway workload, and sandbox
workload jobs. Its full acceptance job was canceled during the test script after
more than nine minutes. Those partial results do not prove a full suite pass.

Both repositories now allow active push runs to finish. Pull-request runs still
cancel obsolete runs for the same workflow and ref. The concurrency group stays
unchanged. There is one active run and at most one pending run per group. A newer
push can replace a pending run. This gives the active revision a complete result
and then checks the latest waiting revision. It does not promise a run for every
intermediate commit. GitHub documents these
[concurrency rules](https://docs.github.com/en/actions/reference/workflows-and-actions/workflow-syntax#concurrency).

The change preserves all test commands, job limits, credentials, service images,
and permissions. YAML parsing and a structural comparison confirmed that only
the cancellation expression changed. Runtime and generated source are unchanged.
The existing STEGO feature run 34497885778 passed its full compiler race suite
and vulnerability check before this workflow change.

Treat cancellation as missing evidence. Report results with their tested commit
and job scope. A focused local test or a passed workload job cannot stand in for
the full application suite. Inspect the current run before deciding to retry it.
A delayed observation or a queued run is not a failed test.
