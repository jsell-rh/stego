// Package idempotency is the inbound idempotency key of the runtime library (ST-SUP-RP07): the migration of the
// idempotency_keys table, the transaction wrapper that records the key with the write and replays the stored
// response, the canonical JSON fingerprint, the derived outgoing key, the purge job and the conformance tests.
//
// It is a library, not a framework (ST-STD-RUNTIME-R01): generated code calls its functions and types, and the
// package calls back into no generated code and owns no main loop.
package idempotency
