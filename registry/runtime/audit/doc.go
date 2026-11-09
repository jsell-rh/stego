// Package audit is the audit record of the runtime library (ST-SUP-RP05): the Event type, the canonical encoder and
// redaction, the SHA-256 hash chain, the RFC 6902 differ, the write inside the caller's transaction, the audit-sink
// port and its outbox destination, the anchor publisher, the verifier and the partition maintenance job.
//
// It is a library, not a framework (ST-STD-RUNTIME-R01): generated code calls its functions and types, and the
// package calls back into no generated code and owns no main loop.
package audit
