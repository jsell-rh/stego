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
also pass in generated forms with and without telemetry. Full compiler CI and
complete Hypershell application results remain required.
