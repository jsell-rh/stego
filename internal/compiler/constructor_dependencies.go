package compiler

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// A dependency name must identify one producer. Routes and middleware can
// still use equal names in separate components because their owner is known.
// Check every constructor, including those that have no current consumer.
func validateConstructorDependencies(wirings []ComponentWiring, packageNames map[string]bool) error {
	producers := make(map[string][]string)
	for _, component := range wirings {
		if component.Wiring == nil {
			continue
		}
		for _, constructor := range component.Wiring.Constructors {
			name := rawConstructorVarName(constructor)
			producers[name] = append(producers[name], component.Name)
		}
	}
	ambiguous := make(map[string]string)
	for name, owners := range producers {
		if len(owners) > 1 {
			sort.Strings(owners)
			ambiguous[name] = name
		}
	}
	if len(ambiguous) == 0 {
		return nil
	}
	for _, component := range wirings {
		w := component.Wiring
		if w == nil {
			continue
		}
		imports := make(map[string]bool)
		for name := range packageNames {
			imports[name] = true
		}
		for _, name := range w.Imports {
			imports[path.Base(name)] = true
		}
		for _, name := range w.StdlibImports {
			imports[path.Base(name)] = true
		}
		for index, constructor := range w.Constructors {
			fail := func(name string) error {
				return fmt.Errorf("component %q constructor %d has ambiguous dependency %q from components %s; use distinct producer constructor names", component.Name, index, name, strings.Join(producers[name], ", "))
			}
			for _, name := range w.ConstructorDeps[index] {
				if ambiguous[name] != "" {
					return fail(name)
				}
			}
			var reference string
			_, err := transformConstructorReferences(constructor, ambiguous, nil, imports, w.ConstructorDeps[index], func(name string) {
				if reference == "" {
					reference = name
				}
			})
			if err != nil {
				return fmt.Errorf("component %q constructor %d: %w", component.Name, index, err)
			}
			if reference != "" {
				return fail(reference)
			}
		}
	}
	return nil
}
