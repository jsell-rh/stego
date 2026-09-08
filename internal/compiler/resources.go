package compiler

import "github.com/jsell-rh/stego/internal/gen"

func constructorUsesResource(wiring *gen.Wiring, index int, want gen.Resource) bool {
	for _, resource := range wiring.ConstructorResources[index] {
		if resource == want {
			return true
		}
	}
	return false
}
