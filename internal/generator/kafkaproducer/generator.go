// Package kafkaproducer is the generator of the kafka-producer component. It emits no code: the Kafka destination
// of the runtime outbox, kafkaoutbox of the pinned github.com/jsell-rh/stego/registry/runtime, publishes each change
// event (ST-SUP-RP01-LC30 to ST-SUP-RP01-LC32). The component marks the service whose change events go to Kafka: the
// outbox wiring registers that destination on the worker (ST-SUP-RC18-OC31), and each generated write of a
// collection stages its change event in the transaction of its command (ST-SUP-RC20-OC40). The owner decided
// ST-SUP-Q103 (a) on 2026-11-11 (ST-SUP-RC12-OC18).
package kafkaproducer

import (
	"errors"

	"github.com/jsell-rh/stego/internal/gen"
)

type Generator struct{}

// MinimumGoVersion is the Go version of the former publisher, so the go directive of a generated module does not
// change; the outbox wiring asks for the Go version of the runtime library that it imports.
func (*Generator) MinimumGoVersion() string { return "1.25.0" }

// ValidateContext checks the output namespace and refuses a service with no outbox component, whose worker delivers
// the change events.
func (*Generator) ValidateContext(ctx gen.Context) error {
	if err := gen.ValidateGoPackageNamespace(ctx.OutputNamespace); err != nil {
		return err
	}
	if ctx.PeerNamespaces["outbox"] == "" {
		return errors.New("kafka-producer: the change events go to Kafka through the worker of the outbox, so the service needs the outbox component (ST-SUP-RC12-OC18)")
	}
	return nil
}

// Generate emits no file and no wiring: the runtime library holds the Kafka destination, and the outbox and rest-api
// components wire it.
func (g *Generator) Generate(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	if err := g.ValidateContext(ctx); err != nil {
		return nil, nil, err
	}
	return nil, &gen.Wiring{}, nil
}
