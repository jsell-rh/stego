// Package ports implements the port resolution algorithm that matches
// component requires to provides, producing a resolved dependency graph.
package ports

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jsell-rh/stego/internal/types"
)

// Resolution maps each component name to its resolved port bindings.
// Each binding maps a required port name to the component that provides it.
type Resolution struct {
	// Bindings maps component name -> port name -> providing component name.
	Bindings map[string]map[string]string
}

// ResolutionError represents a port resolution failure.
type ResolutionError struct {
	Unresolved     []UnresolvedPort
	Ambiguous      []AmbiguousPort
	InvalidBinding []InvalidBinding
}

func (e *ResolutionError) Error() string {
	var parts []string
	for _, ib := range e.InvalidBinding {
		parts = append(parts, ib.Error())
	}
	for _, u := range e.Unresolved {
		if len(u.Providers) > 0 {
			parts = append(parts, fmt.Sprintf("unresolved port %q required by %q: provided by %s but no binding is configured",
				u.Port, u.Component, strings.Join(u.Providers, ", ")))
		} else {
			parts = append(parts, fmt.Sprintf("unresolved port %q required by %q: no component provides it", u.Port, u.Component))
		}
	}
	for _, a := range e.Ambiguous {
		parts = append(parts, fmt.Sprintf("ambiguous port %q required by %q: provided by %s", a.Port, a.Component, strings.Join(a.Providers, ", ")))
	}
	return strings.Join(parts, "; ")
}

func (e *ResolutionError) hasErrors() bool {
	return len(e.Unresolved) > 0 || len(e.Ambiguous) > 0 || len(e.InvalidBinding) > 0
}

// UnresolvedPort describes a required port that has no binding configured.
// Providers lists any components that provide the port (for diagnostic context).
type UnresolvedPort struct {
	Component string
	Port      string
	Providers []string
}

// AmbiguousPort describes a required port provided by more than one component
// with no binding to disambiguate.
type AmbiguousPort struct {
	Component string
	Port      string
	Providers []string
}

// InvalidBinding describes a binding that references a non-existent component,
// a component that does not provide the required port, or a self-referencing
// binding.
type InvalidBinding struct {
	Component string
	Port      string
	BoundTo   string
	Reason    string
}

func (ib InvalidBinding) Error() string {
	return fmt.Sprintf("invalid binding for port %q required by %q: %s", ib.Port, ib.Component, ib.Reason)
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
//
// Ports without an explicit binding are unresolved — a compile error.
// Returns a Resolution on success, or a *ResolutionError listing all
// unresolved, ambiguous, and invalid binding errors.
func Resolve(input ResolveInput) (*Resolution, error) {
	// Merge bindings: archetype defaults, then service overrides on top.
	effectiveBindings := make(map[string]string)
	for port, comp := range input.ArchetypeBindings {
		effectiveBindings[port] = comp
	}
	for port, comp := range input.ServiceOverrides {
		effectiveBindings[port] = comp
	}

	// Build providers index: port name -> list of component names that provide it.
	// Used only for error classification when no binding exists.
	providers := make(map[string][]string)
	for name, comp := range input.Components {
		for _, p := range comp.Provides {
			providers[p.Name] = append(providers[p.Name], name)
		}
	}
	// Sort provider lists for deterministic error messages.
	for port := range providers {
		sort.Strings(providers[port])
	}

	result := &Resolution{
		Bindings: make(map[string]map[string]string),
	}

	var resErr ResolutionError

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

			bound, ok := effectiveBindings[portName]
			if !ok {
				// No binding exists — classify based on how many providers exist.
				// Exclude self from provider list for this check.
				var otherProviders []string
				for _, p := range providers[portName] {
					if p != compName {
						otherProviders = append(otherProviders, p)
					}
				}

				if len(otherProviders) >= 2 {
					resErr.Ambiguous = append(resErr.Ambiguous, AmbiguousPort{
						Component: compName,
						Port:      portName,
						Providers: otherProviders,
					})
				} else {
					resErr.Unresolved = append(resErr.Unresolved, UnresolvedPort{
						Component: compName,
						Port:      portName,
						Providers: otherProviders,
					})
				}
				continue
			}

			// Self-resolution guard: a component cannot provide its own
			// required port.
			if bound == compName {
				resErr.InvalidBinding = append(resErr.InvalidBinding, InvalidBinding{
					Component: compName,
					Port:      portName,
					BoundTo:   bound,
					Reason:    fmt.Sprintf("component %q cannot be bound to itself", compName),
				})
				continue
			}

			// Verify the bound component exists.
			boundComp, exists := input.Components[bound]
			if !exists {
				resErr.InvalidBinding = append(resErr.InvalidBinding, InvalidBinding{
					Component: compName,
					Port:      portName,
					BoundTo:   bound,
					Reason:    fmt.Sprintf("binding references non-existent component %q", bound),
				})
				continue
			}

			// Verify the bound component actually provides this port.
			if !compProvidesPort(boundComp, portName) {
				resErr.InvalidBinding = append(resErr.InvalidBinding, InvalidBinding{
					Component: compName,
					Port:      portName,
					BoundTo:   bound,
					Reason:    fmt.Sprintf("component %q does not provide port %q", bound, portName),
				})
				continue
			}

			bindings[portName] = bound
		}

		if len(bindings) > 0 {
			result.Bindings[compName] = bindings
		}
	}

	if resErr.hasErrors() {
		return nil, &resErr
	}

	return result, nil
}

// compProvidesPort checks whether the component provides the given port.
func compProvidesPort(comp *types.Component, portName string) bool {
	for _, p := range comp.Provides {
		if p.Name == portName {
			return true
		}
	}
	return false
}
