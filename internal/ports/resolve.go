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
	// ActiveComponents is the final set of components after resolution,
	// including any override components loaded from the registry and excluding
	// any archetype default components that were replaced by overrides.
	ActiveComponents map[string]*types.Component
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
	if ib.Component == "" {
		return fmt.Sprintf("invalid binding for port %q: %s", ib.Port, ib.Reason)
	}
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
	// ComponentLoader loads a component by name from the registry. When a
	// service override references a component not in Components, the resolver
	// calls this function to load it. If nil, override components must already
	// be present in Components.
	ComponentLoader func(name string) *types.Component
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
	// Build the active component set: start with input components, then load
	// any override components from the registry and exclude replaced defaults.
	active := make(map[string]*types.Component, len(input.Components))
	for name, comp := range input.Components {
		active[name] = comp
	}

	var resErr ResolutionError

	// Load override components not already in the active set, and exclude
	// archetype defaults that are replaced by overrides.
	// Process override ports in sorted order for deterministic error messages.
	overridePorts := make([]string, 0, len(input.ServiceOverrides))
	for port := range input.ServiceOverrides {
		overridePorts = append(overridePorts, port)
	}
	sort.Strings(overridePorts)

	for _, port := range overridePorts {
		overrideComp := input.ServiceOverrides[port]

		// If the override component is not in the active set, try to load it.
		if _, exists := active[overrideComp]; !exists {
			if input.ComponentLoader == nil {
				resErr.InvalidBinding = append(resErr.InvalidBinding, InvalidBinding{
					Component: "",
					Port:      port,
					BoundTo:   overrideComp,
					Reason:    fmt.Sprintf("override references component %q which is not in the active set and no component loader is configured", overrideComp),
				})
				continue
			}
			loaded := input.ComponentLoader(overrideComp)
			if loaded == nil {
				resErr.InvalidBinding = append(resErr.InvalidBinding, InvalidBinding{
					Component: "",
					Port:      port,
					BoundTo:   overrideComp,
					Reason:    fmt.Sprintf("override component %q not found in registry", overrideComp),
				})
				continue
			}
			active[overrideComp] = loaded
		}

		// Validate the override component provides the overridden port.
		// This check applies to ALL override components — both newly loaded
		// and pre-existing — because a component already in the active set
		// may have been included for a different purpose (e.g. provides
		// "tracing" but not "auth-provider").
		if !compProvidesPort(active[overrideComp], port) {
			resErr.InvalidBinding = append(resErr.InvalidBinding, InvalidBinding{
				Component: "",
				Port:      port,
				BoundTo:   overrideComp,
				Reason:    fmt.Sprintf("override component %q does not provide port %q", overrideComp, port),
			})
			continue
		}

		// Exclude the archetype default component that this override replaces,
		// to prevent both from generating code into the same namespace.
		if defaultComp, ok := input.ArchetypeBindings[port]; ok && defaultComp != overrideComp {
			delete(active, defaultComp)
		}
	}

	// Return early if override loading produced errors.
	if resErr.hasErrors() {
		return nil, &resErr
	}

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
	for name, comp := range active {
		for _, p := range comp.Provides {
			providers[p.Name] = append(providers[p.Name], name)
		}
	}
	// Sort provider lists for deterministic error messages.
	for port := range providers {
		sort.Strings(providers[port])
	}

	result := &Resolution{
		Bindings:         make(map[string]map[string]string),
		ActiveComponents: active,
	}

	// Resolve each component's required ports.
	// Process component names in sorted order for deterministic output.
	componentNames := make([]string, 0, len(active))
	for name := range active {
		componentNames = append(componentNames, name)
	}
	sort.Strings(componentNames)

	for _, compName := range componentNames {
		comp := active[compName]
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

			// Verify the bound component exists in the active set.
			boundComp, exists := active[bound]
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
