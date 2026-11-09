// Package probe is the domain probe of the runtime library (ST-SUP-RP12): the emitter that the generated typed probe
// methods call, which sends each declared observation out through the telemetry runtime and to the local console
// sink, and Recorder, the sink in memory that a fill test asserts emissions on.
//
// It is a library, not a framework (ST-STD-RUNTIME-R01): generated code calls its functions and types, and the
// package calls back into no generated code and owns no main loop.
package probe
