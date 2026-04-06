// Package ports implements the port resolution algorithm that matches
// component requires to provides, producing a resolved dependency graph.
package ports

import (
	"fmt"
	"sort"
	"strings"

	"github.com/stego-project/stego/internal/types"
)

// Resolution maps each component name to its resolved port bindings.
// Each binding maps a required port name to the component that provides it.
type Resolution struct {
	// Bindings maps component name -> port name -> providing component name.
	Bindings map[string]map[string]string
}

// ResolutionError represents a port resolution failure.
type ResolutionError struct {
	Unresolved []UnresolvedPort
	Ambiguous  []AmbiguousPort
}

func (e *ResolutionError) Error() string {
	var parts []string
	for _, u := range e.Unresolved {
		parts = append(parts, fmt.Sprintf("unresolved port %q required by %q: no component provides it", u.Port, u.Component))
	}
	for _, a := range e.Ambiguous {
		parts = append(parts, fmt.Sprintf("ambiguous port %q required by %q: provided by %s", a.Port, a.Component, strings.Join(a.Providers, ", ")))
	}
	return strings.Join(parts, "; ")
}

// UnresolvedPort describes a required port that no component provides.
type UnresolvedPort struct {
	Component string
	Port      string
}

// AmbiguousPort describes a required port provided by more than one component.
type AmbiguousPort struct {
	Component string
	Port      string
	Providers []string
}

// ResolveInput gathers the inputs needed for port resolution.
type ResolveInput struct {
	// Components is the set of all components participating in resolution,
	// keyed by name.
	Components map[string]*types.Component
	// ArchetypeBindings are default port->component mappings from the archetype.
	ArchetypeBindings map[string]string
	// ServiceOverrides are port->component mappings from the service declaration
	// that take precedence over archetype bindings.
	ServiceOverrides map[string]string
}

// Resolve performs port resolution: for each component's required ports, it
// finds exactly one providing component. Resolution order:
//  1. Service-level overrides (highest precedence)
//  2. Archetype default bindings
//  3. Auto-discovery: if exactly one component provides the port, use it
//
// Returns a Resolution on success, or a *ResolutionError listing all
// unresolved and ambiguous ports.
func Resolve(input ResolveInput) (*Resolution, error) {
	// Build a reverse index: port name -> list of components that provide it.
	providers := make(map[string][]string)
	for name, comp := range input.Components {
		for _, port := range comp.Provides {
			providers[port.Name] = append(providers[port.Name], name)
		}
	}
	// Sort provider lists for deterministic error messages.
	for _, list := range providers {
		sort.Strings(list)
	}

	// Merge bindings: archetype defaults, then service overrides on top.
	effectiveBindings := make(map[string]string)
	for port, comp := range input.ArchetypeBindings {
		effectiveBindings[port] = comp
	}
	for port, comp := range input.ServiceOverrides {
		effectiveBindings[port] = comp
	}

	result := &Resolution{
		Bindings: make(map[string]map[string]string),
	}

	var unresolved []UnresolvedPort
	var ambiguous []AmbiguousPort

	// Resolve each component's required ports.
	// Process component names in sorted order for deterministic output.
	componentNames := make([]string, 0, len(input.Components))
	for name := range input.Components {
		componentNames = append(componentNames, name)
	}
	sort.Strings(componentNames)

	for _, compName := range componentNames {
		comp := input.Components[compName]
		if len(comp.Requires) == 0 {
			continue
		}

		bindings := make(map[string]string)
		for _, req := range comp.Requires {
			portName := req.Name

			// Check explicit binding first.
			if bound, ok := effectiveBindings[portName]; ok {
				// Verify the bound component actually provides this port.
				if !componentProvides(input.Components, bound, portName) {
					unresolved = append(unresolved, UnresolvedPort{
						Component: compName,
						Port:      portName,
					})
					continue
				}
				bindings[portName] = bound
				continue
			}

			// Auto-discover: look for exactly one provider.
			providerList := providers[portName]
			switch len(providerList) {
			case 0:
				unresolved = append(unresolved, UnresolvedPort{
					Component: compName,
					Port:      portName,
				})
			case 1:
				bindings[portName] = providerList[0]
			default:
				ambiguous = append(ambiguous, AmbiguousPort{
					Component: compName,
					Port:      portName,
					Providers: providerList,
				})
			}
		}

		if len(bindings) > 0 {
			result.Bindings[compName] = bindings
		}
	}

	if len(unresolved) > 0 || len(ambiguous) > 0 {
		return nil, &ResolutionError{
			Unresolved: unresolved,
			Ambiguous:  ambiguous,
		}
	}

	return result, nil
}

// componentProvides checks whether the named component exists and provides the
// given port.
func componentProvides(components map[string]*types.Component, compName, portName string) bool {
	comp, ok := components[compName]
	if !ok {
		return false
	}
	for _, p := range comp.Provides {
		if p.Name == portName {
			return true
		}
	}
	return false
}
