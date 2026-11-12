// Package outbox generates the durable outbox wiring of a service on github.com/jsell-rh/stego/registry/runtime/outbox
// (runtime.go, ST-SUP-RC18-OC30). The queue table, its policies, its relay functions, the queue and the worker come
// from the runtime library; this package emits only what varies with the declaration.
package outbox

import (
	"github.com/jsell-rh/stego/internal/gen"
)

type Generator struct{}

// MinimumGoVersion stays at the version of the former outbox, so the go directive of a generated module does not
// change; the command and query generators ask for the Go version of the runtime library that the wiring imports.
func (*Generator) MinimumGoVersion() string { return "1.25.0" }

// ValidateContext checks the output namespace and refuses a peer that the outbox on the runtime cannot serve.
func (*Generator) ValidateContext(ctx gen.Context) error {
	if err := gen.ValidateGoPackageNamespace(ctx.OutputNamespace); err != nil {
		return err
	}
	return validateRuntime(ctx)
}

// Generate emits the outbox wiring on the runtime. A service with no entity, no command and no query has no
// generated command whose effects the outbox carries, and gets no file.
func (g *Generator) Generate(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	if err := g.ValidateContext(ctx); err != nil {
		return nil, nil, err
	}
	if len(ctx.Entities) == 0 && len(ctx.Commands) == 0 && len(ctx.Queries) == 0 {
		return nil, &gen.Wiring{}, nil
	}
	return generateRuntime(ctx)
}
