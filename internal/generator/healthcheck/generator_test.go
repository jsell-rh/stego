package healthcheck_test

import (
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/healthcheck"
)

func TestInvalidHealthInputs(t *testing.T) {
	for _, ctx := range []gen.Context{{}, {ModuleName: "example.com/probes", OutputNamespace: "../health"}, {ModuleName: "example.com/probes", OutputNamespace: "health", ComponentConfig: map[string]any{"database": "true"}}, {ModuleName: "example.com/probes", OutputNamespace: "health", ComponentConfig: map[string]any{"databse": true}}} {
		g := new(healthcheck.Generator)
		if err := g.ValidateContext(ctx); err == nil {
			t.Fatal("invalid input accepted")
		}
		if files, _, err := g.Generate(ctx); err == nil || len(files) != 0 {
			t.Fatal("invalid input rendered output")
		}
	}
}
