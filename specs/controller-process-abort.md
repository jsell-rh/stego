The generated controller process must receive a result from its Run callback.
A panic or `runtime.Goexit` must not leave the monitor waiting without a result
or send a private panic value to process output.

`Monitor` now invokes Run with a deferred result report and an explicit return
flag. This covers normal panics, `panic(nil)`, legacy `panic(nil)` behavior,
`runtime.Goexit`, and panics in callback defers. The report discards the panic
value without inspection, formatting, or wrapping. It returns `ErrRunAborted`.
Callback defers finish before the report. The monitor then cancels its context,
closes its HTTP listener, and joins the server. The generated Main reports one
fixed failure event and exits with code 1.

The same callback boundary applies when the local monitor is disabled. Normal
returned errors retain their identity for `errors.Is` and `errors.As`. A callback
must still stop when its context ends. Goroutines started by the callback,
panics in their actions, fatal runtime faults, and external process termination
remain outside this boundary. No automatic retry or forced goroutine termination
is added.

Generated subprocess tests check failure exit codes, private-value exclusion,
deferred cleanup, signal shutdown, and the closed monitor listener. They cover
both telemetry configurations. The application fault build wraps the real
Gateway identity worker Run callback. It runs the domain controller against
real Keycloak, aborts on shutdown, and requires a healthy replacement to repair
identity state. The fault exists only in a temporary Go build overlay.

The generated controller checks passed on jshell on 2026-09-11. Both telemetry
configurations passed under race detection; the generator package took 33.779
seconds. The first fixture attempt failed before runtime tests because Cgo used
the read-only `/tmp` directory. A second command in the same bounded Pod set
`TMPDIR=/work/tmp` and passed. The initial Job still records that first failure;
the successful command has a separate exit result and log. Evidence is retained
in `/tmp/stego-controller-abort-b9182a`. Application adoption is a separate check.
