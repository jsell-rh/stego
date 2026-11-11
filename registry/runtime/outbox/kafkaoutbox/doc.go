// Package kafkaoutbox is the Kafka destination of the outbox runtime (ST-SUP-RP01): a handler that publishes each
// outbox message to Kafka and returns success only when the broker acknowledges it, with its settings from the
// process environment. A service publishes its change events through the outbox, so no event is lost or sent
// without its database change.
//
// It is a library, not a framework (ST-STD-RUNTIME-R01): generated code calls its functions and types, and the
// package calls back into no generated code and owns no main loop.
package kafkaoutbox
