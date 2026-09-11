package compiler

import (
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/grpcapplication"
	"github.com/jsell-rh/stego/internal/generator/kafkaproducer"
	"github.com/jsell-rh/stego/internal/generator/outbox"
	"github.com/jsell-rh/stego/internal/generator/postgresadapter"
	"github.com/jsell-rh/stego/internal/generator/postgresclient"
)

func TestPinnedDependenciesGateGoTarget(t *testing.T) {
	for _, component := range []struct {
		name      string
		minimum   string
		older     string
		generator gen.Generator
	}{
		{"postgres-adapter", "1.25.0", "1.24.9", new(postgresadapter.Generator)},
		{"postgres-client", "1.25.0", "1.24.9", new(postgresclient.Generator)},
		{"outbox", "1.25.0", "1.24.9", new(outbox.Generator)},
		{"kafka-producer", "1.25.0", "1.24.9", new(kafkaproducer.Generator)},
		{"grpc-application", "1.26.0", "1.25.9", new(grpcapplication.Generator)},
		{"grpc-processes", "1.26.0", "1.25.9", new(grpcapplication.ProcessGenerator)},
	} {
		t.Run(component.name, func(t *testing.T) {
			input := snapshotTestInput(t)
			input.GoVersion = component.older
			input.Generators["stub-api"] = rejectedInputGenerator{t}
			input.Generators["stub-store"] = component.generator
			result, err := Validate(input)
			if err != nil || !result.HasErrors() || !strings.Contains(FormatValidation(result), "requires Go "+component.minimum+" or later") {
				t.Fatal("unsupported dependency target passed its check", result, err)
			}
			if plan, err := Reconcile(input); plan != nil || err == nil || !strings.Contains(err.Error(), "requires Go "+component.minimum+" or later") {
				t.Fatal("unsupported dependency target reached generation", plan, err)
			}
			for _, target := range []string{component.minimum, "1.26.8"} {
				if errors := validateGeneratorGoVersion(component.name, component.generator, target); len(errors) != 0 {
					t.Fatal("supported dependency target rejected", target, errors)
				}
			}
		})
	}
}
