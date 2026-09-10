The generated task supervisor must stop the service when a registered task
callback panics or exits without return. A callback abort must not skip peer
cancellation or resource cleanup, or expose the panic value in process output.

Each task invocation has a deferred result report and an explicit return flag.
This covers ordinary panics, `panic(nil)`, panics in callback defers, and
`runtime.Goexit`. It also covers `panic(nil)` with `GODEBUG=panicnil=1`.
The supervisor discards the panic value without formatting or unwrapping it.
It records a fixed internal error, cancels peer tasks, and waits for all task
results. It does not restart the failed callback. Callback defers finish before
the result report. Generated resource cleanup starts after all tasks finish.

The final local `service.failed` record includes sorted `tasks` and
`aborted_tasks` names. `aborted_tasks` contains only callbacks that did not
return. The field is absent when no callback aborted. Task names come from
generated code. Normal returned error causes still support `errors.Is` and
`errors.As`; panic values do not become error causes. The process exits with
code 1 after the existing bounded failure output attempt.

Tasks must still return after context cancellation. The supervisor cannot stop
a goroutine that ignores cancellation. This change adds no forced termination
or new shutdown deadline. It covers the goroutine that invokes each registered
callback. Additional goroutines, constructor panics, cleanup panics, fatal Go
runtime faults, and external process termination remain outside this boundary.
The final failure record is local. Complete process lifecycle export remains
open because telemetry providers close before this record is written.

The Hypershell probe first completed Gateway creation, owner grant storage, and
event delivery. It then caused a task panic. Compiler `8ec724d` exposed the
private panic value. The compiler tests now verify abort detection, private
value handling, cancellation, and deferred cleanup. Generated programs with
and without HTTP pass these checks, including legacy `panic(nil)` behavior.
The full compiler race suite passed with PostgreSQL required on port 32916.
Static checks passed. Application adoption is a separate gate.
