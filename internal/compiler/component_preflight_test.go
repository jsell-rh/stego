package compiler

import (
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/controller"
	"github.com/jsell-rh/stego/internal/generator/jwtauth"
	"github.com/jsell-rh/stego/internal/generator/kafkaproducer"
	"github.com/jsell-rh/stego/internal/generator/kubernetesclient"
	"github.com/jsell-rh/stego/internal/generator/outbox"
	"github.com/jsell-rh/stego/internal/generator/postgresadapter"
	"github.com/jsell-rh/stego/internal/generator/postgresclient"
	"github.com/jsell-rh/stego/internal/generator/restapi"
	"github.com/jsell-rh/stego/internal/generator/tslsearch"
	"github.com/jsell-rh/stego/internal/types"
)

func TestComponentPreflightMatchesDirectGeneration(t *testing.T) {
	for _, test := range []struct {
		name      string
		generator gen.Generator
		ctx       gen.Context
		want      string
	}{
		{"controller settings", new(controller.Generator), gen.Context{OutputNamespace: "controller", ComponentConfig: map[string]any{"unexpected": true}}, "no component settings"},
		{"Kubernetes transport", new(kubernetesclient.Generator), gen.Context{OutputNamespace: "kube", ModuleName: "example.com/service"}, "requires the generated HTTP"},
		{"PostgreSQL client path", new(postgresclient.Generator), gen.Context{OutputNamespace: "../client"}, "canonical relative path"},
		{"outbox path", new(outbox.Generator), gen.Context{OutputNamespace: "../outbox"}, "canonical relative path"},
		{"search metadata", new(tslsearch.Generator), gen.Context{Entities: []types.Entity{{Name: "Record", Fields: []types.Field{{Name: "created_at", Type: types.FieldTypeString}}}}}, "conflicts with common metadata"},
		{"storage reserved table", new(postgresadapter.Generator), gen.Context{Entities: []types.Entity{{Name: "StegoScanCheckpoint"}}}, "internal checkpoint table"},
		{"JWT header", new(jwtauth.Generator), gen.Context{ComponentConfig: map[string]any{"header": "Bad Header"}}, "invalid authentication header"},
		{"JWT claim", new(jwtauth.Generator), gen.Context{ComponentConfig: map[string]any{"roles_claim": "roles..name"}}, "dotted claim path"},
		{"REST observation owner", new(restapi.Generator), gen.Context{Entities: []types.Entity{{Name: "Record", Observations: map[string][]string{"worker": {"status"}}}}, Collections: []types.Collection{{Name: "records", Entity: "Record"}}}, "observation ownership"},
		{"Kafka outbox", new(kafkaproducer.Generator), gen.Context{OutputNamespace: "publisher", StorageContract: "example.com/storage"}, "requires the outbox"},
	} {
		t.Run(test.name, func(t *testing.T) {
			validator, ok := test.generator.(gen.ContextValidator)
			if !ok {
				t.Fatal("component has no preflight check")
			}
			checked := validator.ValidateContext(test.ctx)
			if checked == nil || !strings.Contains(checked.Error(), test.want) {
				t.Fatal("preflight accepted invalid inputs", checked)
			}
			files, wiring, err := test.generator.Generate(test.ctx)
			if err == nil || err.Error() != checked.Error() || files != nil || wiring != nil {
				t.Fatal("direct generation did not use the same input check", err)
			}
		})
	}
}
