// Package lease is the lease client of the runtime library (ST-SUP-RP09): a lease with fencing tokens. The claim loop
// for outbox and job rows is in package outbox, not here.
//
// It is a library, not a framework (ST-STD-RUNTIME-R01): generated code calls its functions and types, and the
// package calls back into no generated code and owns no main loop.
package lease
