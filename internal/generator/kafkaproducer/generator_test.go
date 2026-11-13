package kafkaproducer

import (
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
)

func TestKafkaProducer(t *testing.T) {
	t.Run("ST-SUP-RC12-OC18-T01 kafka-producer emits no code and needs the outbox", func(t *testing.T) {
		ctx := gen.Context{OutputNamespace: "internal/events", ModuleName: "example.com/kafka-test",
			PeerNamespaces: map[string]string{"outbox": "internal/outbox"}}
		files, wiring, err := new(Generator).Generate(ctx)
		if err != nil {
			t.Fatalf("kafka-producer with the outbox was refused: %v", err)
		}
		if len(files) != 0 || wiring == nil || len(wiring.GoModRequires) != 0 || len(wiring.Constructors) != 0 || len(wiring.Imports) != 0 {
			t.Fatalf("kafka-producer gave %d files and the wiring %+v, want no file, no module and no constructor", len(files), wiring)
		}
		ctx.PeerNamespaces = nil
		for name, check := range map[string]func() error{
			"ValidateContext": func() error { return new(Generator).ValidateContext(ctx) },
			"Generate": func() error {
				_, _, err := new(Generator).Generate(ctx)
				return err
			},
		} {
			if err := check(); err == nil || !strings.Contains(err.Error(), "outbox component") {
				t.Errorf("%s with no outbox gave %v, want a refusal that names the outbox component", name, err)
			}
		}
	})
}
