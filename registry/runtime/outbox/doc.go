// Package outbox is the transactional outbox of the runtime library (ST-SUP-RP01): the queue table, the claim and
// lease protocol, the dispatch states for an outcome that is not known, the destination registration with its
// idempotency and status lookup, the delivery worker and the live event source.
//
// It is a library, not a framework (ST-STD-RUNTIME-R01): generated code calls its functions and types, and the
// package calls back into no generated code and owns no main loop.
package outbox
