The generated controller process now starts common telemetry before it calls
the application worker. Previously, only a reconciliation loop owned telemetry.
Provider setup could fail before that loop started. Setup and cleanup calls
could also receive a context with no runtime.

With an `otel-tracing` peer, `Monitor` owns one reference through the callback's
complete lifetime. Setup, nested controllers, and deferred application cleanup
share the same private runtime and instance identity. Nested controllers retain
their existing ownership references. Their completion cannot close the worker's
runtime. The last owner flushes and closes it with the existing bounded policy.

A callback error or abort emits the fixed `service.failed` event before close.
The runtime does not format the error or panic value. Cancellation does not emit
that failure event. The callback's return value and error identity are retained.
The process still writes its fixed local failure record and returns its existing
exit code. A callback must still stop and join its work when its context ends.

Probe commands do not start telemetry or call application code. Invalid monitor
addresses and failures before runtime creation retain their local failure path.
The runtime does not claim application readiness from callback entry. This
change does not complete API bootstrap, RPC initialization, or domain telemetry.
A controller without a telemetry peer retains its standalone behavior.

The focused generated regression failed on the old implementation: setup and
cleanup signals were absent, and two nested controller runs used two runtime
identities. With this change, it passes for success, failure, cancellation,
panic, and `runtime.Goexit`. It verifies one runtime, setup and cleanup SQL
signals, nested controller signals, preserved error identity, private-data
exclusion, and released ownership. Existing signal, probe, and failure checks
also pass in generated forms with and without telemetry. Complete Hypershell
application results remain required.

[Full compiler CI](https://github.com/jsell-rh/stego/actions/runs/34964671704)
passed at `f97b315`, including the generated controller race suite in 36.465
seconds and real PostgreSQL provisioning. This verifies the shared compiler
change; it does not replace the complete application gates.

Hypershell `677973f` adopts compiler `f97b315`. Its startup test reproduced the
missing local and OTLP events on the previous source. The updated test passed
for all four generated workers in 3.887 seconds. Each process failed during
provider setup, excluded private data, and exported one start, failure, and stop
record with the same identity as local output. This is a focused startup check;
the complete API and browser workflows remain queued.

The [complete API evidence](hypershell-worker-startup-api.json) records a pass for Hypershell `677973f`
in run `34964891352`. All 31 required tests passed; acceptance took 166.615
seconds. This run includes the four actual worker startup failures and their
correlated local and TLS OTLP records, in 7.24 seconds. Verification matched
907 source files, 230 generated files, and four identical generation records.
Automatic cleanup and independent cluster reads found no test resources. The
complete browser run `34964891373` and full CI `34964891409` remain active.
