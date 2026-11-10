This component generates the typed domain probes of a service. Each declared
observation gets one typed method. Each command, query, worker, process method
and slot binding gets one probe type, with the methods of the observations that
name it in Emitted by and no other method.

A method takes loaded entity values, enum constants, bools and measures. It has
no free text, no context and no result. It calls the probe runtime
(stego.dev/runtime/probe) only, which sends the observation to the telemetry
runtime and to the local console sink.

The component also generates a constructor for fill tests on probe.Recorder for
each probe type, the context accessor of each slot binding, and the catalog
observations.json, which lists each observation in name order.

The compiler gives the generator the observations of service.yaml and its
emitters: the commands, queries, workers and slot bindings (as
collection.slot). The stage is alpha.
