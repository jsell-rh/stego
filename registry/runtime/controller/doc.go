// Package controller is the reconciliation loop of the runtime library (ST-SUP-RP02): the keyed and FIFO queues, the
// scan and the stream of sources, retries, sweeps, the metrics collector and the telemetry of each operation.
//
// It is a library, not a framework (ST-STD-RUNTIME-R01): generated code calls its functions and types, and the
// package calls back into no generated code and owns no main loop.
package controller
