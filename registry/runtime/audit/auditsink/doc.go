// Package auditsink gives the systems that the audit-sink port of a service binds (ST-SUP-RP05): an HTTPS endpoint
// that takes OpenTelemetry log records over OTLP/HTTP, when the deployment names one, and else a table of its own
// in the PostgreSQL database that the service already uses. The worker mode reads its settings and gives the sink to
// the outbox destination of the audit sink; no handler reaches it.
//
// It is a library, not a framework (ST-STD-RUNTIME-R01): generated code calls its functions and types, and the
// package calls back into no generated code and owns no main loop.
package auditsink
