// Package modes is the composition of one binary in the runtime library (ST-SUP-RP09): the mode registry, the flag
// parsing, and the readiness and the shutdown sequencer of each mode.
//
// It is a library, not a framework (ST-STD-RUNTIME-R01): generated code calls its functions and types, and the
// package calls back into no generated code and owns no main loop.
package modes
