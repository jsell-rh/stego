package registry

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jsell-rh/stego/internal/types"
)

// sunsetLayout is how a sunset date is written: a calendar date.
const sunsetLayout = time.DateOnly

// FitForGoldenPath holds for a component whose stage is ga (ST-SUP-SP01).
// An archetype or a mixin may include only a component for which it holds.
func FitForGoldenPath(component *Component) bool {
	return component != nil && component.Stage == types.StageGA
}

// PastSunset holds for an alpha or beta component whose sunset date is
// earlier than today (ST-SUP-SP04). The sunset of today is not past, and a ga
// component is never past its sunset. The caller gives today; the predicate
// reads no clock.
func PastSunset(component *Component, today time.Time) bool {
	if component == nil || (component.Stage != types.StageAlpha && component.Stage != types.StageBeta) {
		return false
	}
	sunset, err := time.Parse(sunsetLayout, component.Sunset)
	if err != nil {
		return false
	}
	year, month, day := today.Date()
	return sunset.Before(time.Date(year, month, day, 0, 0, 0, 0, time.UTC))
}

// checkStage refuses a component that states no valid stage, an alpha or
// beta component with no valid sunset date, and a ga component that states a
// sunset date (ST-SUP-UC04-R01, ST-SUP-UC05-R03).
func checkStage(component *Component) error {
	if _, err := types.ParseStage(string(component.Stage)); err != nil {
		return fmt.Errorf("component %s: %w", component.Name, err)
	}
	if component.Stage == types.StageGA {
		if component.Sunset != "" {
			return fmt.Errorf("component %s is ga and states the sunset date %s; a ga component has no sunset date", component.Name, component.Sunset)
		}
		return nil
	}
	if component.Sunset == "" {
		return fmt.Errorf("component %s is %s and its sunset date is missing; an alpha or beta component must state the date it stops working", component.Name, component.Stage)
	}
	if _, err := time.Parse(sunsetLayout, component.Sunset); err != nil {
		return fmt.Errorf("component %s: sunset date %q is not a date of the form 2006-01-02", component.Name, component.Sunset)
	}
	return nil
}

// checkGoldenPaths refuses an archetype or a mixin that includes a component
// that is not fit for a golden path, naming each such component with its
// stage (ST-SUP-SP01).
func checkGoldenPaths(r *Registry) error {
	var faults []string
	check := func(kind, name string, components []string) {
		var below []string
		for _, included := range components {
			if component := r.components[included]; component != nil && !FitForGoldenPath(component) {
				below = append(below, included+" ("+string(component.Stage)+")")
			}
		}
		if len(below) > 0 {
			faults = append(faults, fmt.Sprintf("%s %s includes components that are not ga: %s", kind, name, strings.Join(below, ", ")))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(r.archetypes)) {
		check("archetype", name, r.archetypes[name].Components)
	}
	for _, name := range slices.Sorted(maps.Keys(r.mixins)) {
		check("mixin", name, r.mixins[name].AddsComponents)
	}
	if len(faults) > 0 {
		return fmt.Errorf("%s", strings.Join(faults, "; "))
	}
	return nil
}

// checkExperimentPorts refuses a component below ga that provides a port a
// ga component provides, naming the component, its stage, the port and the
// ga component (ST-SUP-INV03).
func checkExperimentPorts(r *Registry) error {
	gaProviders := map[string][]string{}
	for _, name := range slices.Sorted(maps.Keys(r.components)) {
		if component := r.components[name]; component.Stage == types.StageGA {
			for _, port := range component.Provides {
				gaProviders[port.Name] = append(gaProviders[port.Name], name)
			}
		}
	}
	var faults []string
	for _, name := range slices.Sorted(maps.Keys(r.components)) {
		component := r.components[name]
		if component.Stage == types.StageGA {
			continue
		}
		for _, port := range component.Provides {
			if providers := gaProviders[port.Name]; len(providers) > 0 {
				faults = append(faults, fmt.Sprintf("component %s (%s) provides the port %s, which the ga component %s provides; an experiment owns only its own port", name, component.Stage, port.Name, strings.Join(providers, ", ")))
			}
		}
	}
	if len(faults) > 0 {
		return fmt.Errorf("%s", strings.Join(faults, "; "))
	}
	return nil
}
