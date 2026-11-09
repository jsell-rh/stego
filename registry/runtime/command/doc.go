// Package command is the command handle of the runtime library (ST-SUP-RP10): the handle for slots bound to one store
// transaction, tenant and principal; Run, which calls a command fill inside the idempotency wrapper and ends the
// transaction; and RunWorker and RunConsumer, which call a worker fill with the same handle.
//
// It is a library, not a framework (ST-STD-RUNTIME-R01): generated code calls its functions and types, and the
// package calls back into no generated code and owns no main loop.
package command
