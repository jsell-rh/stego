// Package healthcheck provides a no-op Generator stub for the health-check
// component. A full generator is post-MVP; this stub satisfies the archetype's
// component list so that the registry, port resolution, and compiler don't
// error on missing components.
package healthcheck

import "github.com/stego-project/stego/internal/gen"

// Generator is a no-op code generator for the health-check component.
type Generator struct{}

// Generate returns an empty file list and nil wiring.
func (g *Generator) Generate(_ gen.Context) ([]gen.File, *gen.Wiring, error) {
	return nil, nil, nil
}
