// Package kafkaproducer generates a Kafka publisher with verified TLS.
package kafkaproducer

import (
	_ "embed"
	"fmt"
	"go/format"
	"path"
	"strings"

	"github.com/jsell-rh/stego/internal/gen"
)

//go:embed publisher.go.tmpl
var publisherSource string

type Generator struct{}

// MinimumGoVersion covers the pinned Kafka dependency.
func (*Generator) MinimumGoVersion() string { return "1.25.0" }

// ValidateContext checks the publisher path. It refuses a service that has the outbox component: the delivery of
// outbox messages to Kafka ran on the worker of the former outbox, which STEGO no longer generates
// (ST-SUP-RC20-OC39), and kafka-producer as a destination of the runtime outbox is not wired yet (ST-SUP-Q103).
func (*Generator) ValidateContext(ctx gen.Context) error {
	if err := gen.ValidateGoPackageNamespace(ctx.OutputNamespace); err != nil {
		return err
	}
	if ctx.PeerNamespaces["outbox"] != "" {
		return fmt.Errorf("kafka-producer with the outbox component is not served: its delivery of outbox messages ran on the worker of the former outbox, which STEGO no longer generates (ST-SUP-RC20-OC39), and kafka-producer as a destination of the runtime outbox is not wired yet (ST-SUP-Q103); remove the event-publisher mixin or the kafka-producer component")
	}
	return nil
}

// Generate emits the publisher alone.
func (g *Generator) Generate(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	if err := g.ValidateContext(ctx); err != nil {
		return nil, nil, err
	}
	ns := ctx.OutputNamespace
	source, err := format.Source([]byte(strings.Replace(publisherSource, "package publisher", "package "+path.Base(ns), 1)))
	if err != nil {
		return nil, nil, fmt.Errorf("formatting Kafka publisher: %w", err)
	}
	files := []gen.File{{Path: path.Join(ns, "publisher.go"), Content: source}}
	wiring := &gen.Wiring{GoModRequires: map[string]string{
		"github.com/twmb/franz-go": "v1.21.6", "github.com/google/uuid": "v1.6.0",
	}}
	if err := gen.ValidateNamespace(ns, files); err != nil {
		return nil, nil, err
	}
	return files, wiring, nil
}
