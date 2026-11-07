package compiler

import (
	"errors"
	"fmt"
	"go/version"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/jsell-rh/stego/internal/declaration"
	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/ports"
	"github.com/jsell-rh/stego/internal/registry"
	"github.com/jsell-rh/stego/internal/types"
)

// ValidationError is one fault of the check, as Declaring's fault list holds it (ST-DECL-RM01).
type ValidationError = declaration.ValidationError

// ValidationResult is every fault the check finds.
type ValidationResult = declaration.ValidationResult

// FormatValidation renders the faults for a person.
func FormatValidation(r *ValidationResult) string { return declaration.FormatValidation(r) }

// Validate checks the service declaration against the registry and reports all
// semantic validation errors without rendering output. Declaration and registry
// loading failures are returned as Go errors. Component input failures and
// semantic issues are collected in ValidationResult.Errors.
func Validate(input ReconcilerInput) (*ValidationResult, error) {
	source, err := loadCompilationSource(input)
	var unreadable *declarationReadError
	if errors.As(err, &unreadable) {
		return &ValidationResult{Errors: []ValidationError{declaration.ReadingFault(unreadable.err)}}, nil
	}
	if err != nil {
		return nil, err
	}
	return validateSource(input, source)
}

// validateSource is the common semantic gate for validate, plan, and apply.
func validateSource(input ReconcilerInput, source *compilationSource) (*ValidationResult, error) {
	result := &ValidationResult{}
	if err := validateBuildTarget(input.ModuleName, input.GoVersion); err != nil {
		result.Errors = append(result.Errors, ValidationError{Category: "target", Rule: "build-target", Path: "build target", Found: input.ModuleName + " go" + input.GoVersion, Message: err.Error()})
	}
	svcDecl, reg := source.Service, source.Registry

	// Validate archetype exists.
	archetype := reg.Archetype(svcDecl.Archetype)
	if archetype == nil && svcDecl.Archetype != "" {
		result.Errors = append(result.Errors, ValidationError{
			Category: "archetype", Rule: "archetype", Path: "archetype", Found: svcDecl.Archetype, Allowed: archetypeNames(reg),
			Message: fmt.Sprintf("archetype %q not found in registry", svcDecl.Archetype),
		})
	}

	// Validate language: must match archetype's language and only "go" is
	// supported in MVP.
	if archetype != nil {
		result.Errors = append(result.Errors, validateLanguage(svcDecl.Language, archetype.Language)...)
	}

	// Component and port validation require a valid archetype.
	var components map[string]*registry.Component
	var baselineNames []string
	if archetype != nil {
		var faults []ValidationError
		baselineNames, components, faults = activeSet(archetype, svcDecl, reg)
		result.Errors = append(result.Errors, faults...)
	}

	own := declaration.Validate(svcDecl, source.Undeclared...)
	result.Errors = append(result.Errors, own.Undeclared...)
	result.Errors = append(result.Errors, own.Service...)
	result.Errors = append(result.Errors, applyComponentNamespaces(svcDecl.ComponentNamespaces, components)...)
	if components != nil {
		result.Errors = append(result.Errors, validateComponentConfig(svcDecl, components)...)
		result.Errors = append(result.Errors, validateComponentNamespaces(components)...)
		for _, name := range sortedKeys(components) {
			if !generatorAvailable(input.Generators[name]) {
				result.Errors = append(result.Errors, ValidationError{
					Category: "generator", Rule: "generator", Path: "archetype", Found: name,
					Message: fmt.Sprintf("component %q has no generator in this compiler build", name),
				})
				continue
			}
			result.Errors = append(result.Errors, validateGeneratorGoVersion(name, input.Generators[name], input.GoVersion)...)
		}
	}

	result.Errors = append(result.Errors, own.Entities...)
	for index, entity := range svcDecl.Entities {
		if entity.Versioned {
			if _, supported := components["postgres-adapter"]; !supported {
				result.Errors = append(result.Errors, ValidationError{Category: "component", Rule: "versioned-store", Path: fmt.Sprintf("entities[%d].versioned", index), Found: "true", Message: fmt.Sprintf("versioned entity %s requires postgres-adapter", entity.Name)})
			}
		}
	}
	result.Errors = append(result.Errors, own.Collections...)

	// Validate slot bindings reference available slots.
	if components != nil {
		// Collect mixin-added slots (adds_slots) — these are defined by the
		// mixin itself, not by any component, so they must be added to the
		// available set separately.
		var mixinSlots []registry.SlotDefinition
		for _, mixinName := range svcDecl.Mixins {
			mixin := reg.Mixin(mixinName)
			if mixin != nil {
				mixinSlots = append(mixinSlots, mixin.AddsSlots...)
			}
		}
		result.Errors = append(result.Errors, validateSlotNames(svcDecl.Slots, components, mixinSlots)...)
		result.Errors = append(result.Errors, validateSlotConsumption(svcDecl.Slots, input.Generators)...)
	}

	result.Errors = append(result.Errors, own.Slots...)
	if err := validateSlotVarNameUniqueness(svcDecl.Slots); err != nil {
		result.Errors = append(result.Errors, ValidationError{Category: "slot-binding", Rule: "slot-variable-name", Path: "slots", Message: err.Error()})
	}

	// Validate fills exist on disk and reference valid collections.
	fillErrs, fillInfraErr := validateFillsExist(svcDecl.Slots, svcDecl.Collections, input.ProjectDir, input.ReadFill)
	if fillInfraErr != nil {
		return nil, fillInfraErr
	}
	result.Errors = append(result.Errors, fillErrs...)

	if components != nil {
		source.Active = components
	}

	if !result.HasErrors() {
		resolved, err := prepareComponents(input, source, baselineNames, components)
		if err != nil {
			result.Errors = append(result.Errors, ValidationError{Category: "generator", Rule: "generator-input", Path: "archetype", Found: svcDecl.Archetype, Message: err.Error()})
		} else {
			source.Resolved = resolved
		}
	}
	return result, nil
}

func validateGeneratorGoVersion(name string, generator gen.Generator, target string) []ValidationError {
	requirement, ok := generator.(gen.GoVersionRequirement)
	if !ok {
		return nil
	}
	minimum := requirement.MinimumGoVersion()
	var message string
	switch {
	case !version.IsValid("go" + minimum):
		message = fmt.Sprintf("component %q declares an invalid minimum Go version %q", name, minimum)
	case !version.IsValid("go" + target):
		message = fmt.Sprintf("component %q requires a valid Go target, got %q", name, target)
	case version.Compare("go"+target, "go"+minimum) < 0:
		message = fmt.Sprintf("component %q requires Go %s or later; target is %s", name, minimum, target)
	default:
		return nil
	}
	return []ValidationError{{Category: "generator", Rule: "generator-go-version", Path: "build target", Found: target, Message: message}}
}

// Reject typed nil implementations as well as missing map entries.
func generatorAvailable(generator gen.Generator) bool {
	if generator == nil {
		return false
	}
	value := reflect.ValueOf(generator)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return !value.IsNil()
	default:
		return true
	}
}

// validateSlotNames checks that each slot binding references a slot defined by
// one of the resolved components or by a mixin's adds_slots.
func validateSlotNames(slots []types.SlotDeclaration, components map[string]*registry.Component, mixinSlots []registry.SlotDefinition) []ValidationError {
	// Build set of available slot names across all components.
	available := make(map[string]bool)
	for _, comp := range components {
		for _, sd := range comp.Slots {
			available[sd.Name] = true
		}
	}

	// Include mixin-added slots (adds_slots). These are defined directly
	// by mixins, not by any component — collectComponentNames only adds
	// the mixin's AddsComponents, not its AddsSlots.
	for _, sd := range mixinSlots {
		available[sd.Name] = true
	}

	var errs []ValidationError
	for si, sb := range slots {
		if !available[sb.Slot] {
			// Collect available slot names for a helpful error message.
			var names []string
			for name := range available {
				names = append(names, name)
			}
			sort.Strings(names)
			errs = append(errs, ValidationError{
				Category: "slot", Rule: "slot-name", Path: fmt.Sprintf("slots[%d].slot", si), Found: sb.Slot, Allowed: names,
				Message: fmt.Sprintf("slot binding references slot %q which is not defined by any component (available slots: %v)", sb.Slot, names),
			})
		}
	}
	return errs
}

// validateSlotConsumption checks that every bound slot is consumed by at
// least one resolved generator. A binding to an unconsumed slot wires an
// operator that no generated code ever calls, so the bound fills would be
// silent dead code.
func validateSlotConsumption(slots []types.SlotDeclaration, generators map[string]gen.Generator) []ValidationError {
	if len(slots) == 0 || len(generators) == 0 {
		return nil
	}
	consumed := make(map[string]bool)
	for _, generator := range generators {
		consumer, ok := generator.(gen.SlotConsumer)
		if !ok || !generatorAvailable(generator) {
			// An unavailable generator already errors elsewhere. A generator
			// that does not implement SlotConsumer consumes no slots.
			continue
		}
		for _, slot := range consumer.ConsumedSlots() {
			consumed[slot] = true
		}
	}
	var errs []ValidationError
	for si, sb := range slots {
		if consumed[sb.Slot] {
			continue
		}
		var available []string
		for name := range consumed {
			available = append(available, name)
		}
		sort.Strings(available)
		errs = append(errs, ValidationError{
			Category: "slot", Rule: "unconsumed-slot", Path: fmt.Sprintf("slots[%d].slot", si), Found: sb.Slot, Allowed: available,
			Message: fmt.Sprintf("slot binding references slot %q which no resolved generator consumes (consumed slots: %v)", sb.Slot, available),
		})
	}
	return errs
}

// validateFillsExist checks that each fill referenced in slot bindings has a
// fills/<name>/fill.yaml file in the project directory and that the fill's
// Collection field references a defined collection name. It returns validation
// errors for missing fills or invalid collection references, and returns an
// infrastructure Go error for unexpected filesystem or parsing failures.
func validateFillsExist(slots []types.SlotDeclaration, collections []types.Collection, projectDir string, readFill FillReader) ([]ValidationError, error) {
	fillNames := collectFillNames(slots)
	if len(fillNames) == 0 {
		return nil, nil
	}

	collectionNames := make(map[string]bool, len(collections))
	for _, c := range collections {
		collectionNames[c.Name] = true
	}

	var errs []ValidationError
	for _, name := range fillNames {
		fillPath := filepath.Join(projectDir, "fills", name, "fill.yaml")
		if _, err := os.Stat(fillPath); err != nil {
			if os.IsNotExist(err) {
				errs = append(errs, ValidationError{
					Category: "fill", Rule: "fill-exists", Path: fillReference(slots, name), Found: name,
					Message: fmt.Sprintf("fill %q referenced in slot bindings but fills/%s/fill.yaml does not exist", name, name),
				})
			} else {
				return nil, fmt.Errorf("checking fill %q at %s: %w", name, fillPath, err)
			}
			continue
		}

		// Parse the fill and validate its Collection field.
		if readFill == nil {
			return nil, fmt.Errorf("parsing fill %q at %s: the compiler was given no fill reader", name, fillPath)
		}
		collection, err := readFill(fillPath)
		if err != nil {
			return nil, fmt.Errorf("parsing fill %q at %s: %w", name, fillPath, err)
		}
		if collection != "" && !collectionNames[collection] {
			var names []string
			for _, c := range collections {
				names = append(names, c.Name)
			}
			sort.Strings(names)
			errs = append(errs, ValidationError{
				Category: "fill", Rule: "fill-collection", Path: fillReference(slots, name), Found: collection, Allowed: names,
				Message: fmt.Sprintf("fill %q references collection %q which is not defined (available collections: %v)", name, collection, names),
			})
		}
	}
	return errs, nil
}

// ResolveFillCheck reads the declaration and the active registry and gives
// what a check of the fills needs: each slot of the active component set and
// of the declaration's mixins, each fill that a slot binding names, and the
// declared collections. It does not check the fills.
func ResolveFillCheck(input ReconcilerInput) (FillCheck, error) {
	source, err := loadCompilationSource(input)
	if err != nil {
		return FillCheck{}, err
	}
	result, err := validateSource(input, source)
	if err != nil {
		return FillCheck{}, err
	}
	if source.Active == nil {
		return FillCheck{}, fmt.Errorf("the active component set cannot be resolved:\n%s", FormatValidation(result))
	}
	return fillCheck(input.ProjectDir, source.Service, source.Registry, source.Active), nil
}

// fillCheck gives the fill check of a declaration with its active component
// set.
func fillCheck(projectDir string, svcDecl *types.ServiceDeclaration, reg *registry.Registry, components map[string]*registry.Component) FillCheck {
	check := FillCheck{ProjectDir: projectDir, Contracts: reg}
	for _, name := range sortedKeys(components) {
		for _, sd := range components[name].Slots {
			check.Slots = append(check.Slots, FillSlot{
				Ref:      types.SlotRef{Component: name, Slot: sd.Name},
				Contract: "components/" + name + "/slots/" + sd.Name + ".proto",
			})
		}
	}
	for _, mixinName := range svcDecl.Mixins {
		mixin := reg.Mixin(mixinName)
		if mixin == nil {
			continue
		}
		for _, sd := range mixin.AddsSlots {
			check.Slots = append(check.Slots, FillSlot{
				Ref:      types.SlotRef{Component: mixin.Name, Slot: sd.Name},
				Contract: "mixins/" + mixin.Name + "/slots/" + sd.Name + ".proto",
			})
		}
	}
	for _, c := range svcDecl.Collections {
		check.Collections = append(check.Collections, c.Name)
	}
	for _, sb := range svcDecl.Slots {
		for _, list := range [][]string{sb.Gate, sb.Chain, sb.FanOut} {
			for _, name := range list {
				check.Bindings = append(check.Bindings, FillBinding{Fill: name, Slot: sb.Slot, Collection: sb.Collection})
			}
		}
	}
	return check
}

// validateLanguage checks that the service declaration's language matches the
// archetype's declared language and that only supported languages are used.
// Only "go" is supported in MVP.
func validateLanguage(serviceLanguage, archetypeLanguage string) []ValidationError {
	var errs []ValidationError

	if serviceLanguage == "" {
		errs = append(errs, ValidationError{
			Category: "language", Rule: "language", Path: "language", Allowed: []string{archetypeLanguage},
			Message: "service declaration is missing the 'language' field",
		})
		return errs
	}

	if serviceLanguage != "go" {
		errs = append(errs, ValidationError{
			Category: "language", Rule: "language", Path: "language", Found: serviceLanguage, Allowed: []string{"go"},
			Message: fmt.Sprintf("unsupported language %q — only 'go' is supported", serviceLanguage),
		})
	}

	if archetypeLanguage != "" && serviceLanguage != archetypeLanguage {
		errs = append(errs, ValidationError{
			Category: "language", Rule: "language", Path: "language", Found: serviceLanguage, Allowed: []string{archetypeLanguage},
			Message: fmt.Sprintf("service language %q does not match archetype language %q", serviceLanguage, archetypeLanguage),
		})
	}

	return errs
}

// archetypeNames lists the archetypes the registry holds, in name order.
func archetypeNames(reg *registry.Registry) []string {
	names := make([]string, 0, len(reg.Archetypes()))
	for name := range reg.Archetypes() {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// fillReference gives the path of the first slot binding place that names the fill.
func fillReference(slots []types.SlotDeclaration, name string) string {
	for i, binding := range slots {
		for _, operator := range []struct {
			name  string
			fills []string
		}{{"gate", binding.Gate}, {"chain", binding.Chain}, {"fan-out", binding.FanOut}} {
			for j, fill := range operator.fills {
				if fill == name {
					return fmt.Sprintf("slots[%d].%s[%d]", i, operator.name, j)
				}
			}
		}
	}
	return "slots"
}

// activeSet resolves the active component set of a declaration, as checking and generation read it: the archetype's
// components, its default auth and the declared mixins' components, with the archetype's port bindings and the
// declaration's port overrides. It gives the baseline names, and the active set when every component is found and
// the ports resolve; else the faults.
func activeSet(archetype *registry.Archetype, svcDecl *types.ServiceDeclaration, reg *registry.Registry) ([]string, map[string]*registry.Component, []ValidationError) {
	var faults []ValidationError
	baselineNames, err := collectComponentNames(archetype, svcDecl, reg)
	if err != nil {
		return baselineNames, nil, []ValidationError{{
			Category: "component", Rule: "component", Path: "mixins",
			Message: err.Error(),
		}}
	}
	baselineComponents := make(map[string]*registry.Component)
	for _, name := range baselineNames {
		comp := reg.Component(name)
		if comp == nil {
			faults = append(faults, ValidationError{
				Category: "component", Rule: "component", Path: "archetype", Found: name,
				Message: fmt.Sprintf("component %q not found in registry (referenced by archetype %q)", name, archetype.Name),
			})
			continue
		}
		baselineComponents[name] = comp
	}
	// Port resolution needs every baseline component.
	if len(faults) > 0 {
		return baselineNames, nil, faults
	}
	servicePortOverrides := make(map[string]string)
	for key, val := range svcDecl.Overrides {
		if strVal, ok := val.(string); ok {
			if declaration.IsConventionOverrideKey(key) {
				continue
			}
			servicePortOverrides[key] = strVal
		}
	}
	resolution, portErr := ports.Resolve(ports.ResolveInput{
		Components:        baselineComponents,
		ArchetypeBindings: archetype.Bindings,
		ServiceOverrides:  servicePortOverrides,
		ComponentLoader:   reg.Component,
	})
	if portErr != nil {
		return baselineNames, nil, []ValidationError{{
			Category: "port", Rule: "port-binding", Path: "overrides",
			Message: portErr.Error(),
		}}
	}
	return baselineNames, resolution.ActiveComponents, nil
}

// ActiveComponents gives the active component set of an archetype for a declaration that adds nothing to it: the
// set whose settings and slots checking reads, so that the report of what may be declared (ST-SUP-QRY01) and
// checking cannot disagree. It refuses an archetype the registry does not hold, naming the archetypes it holds.
func ActiveComponents(reg *registry.Registry, archetypeName string) (map[string]*registry.Component, error) {
	archetype := reg.Archetype(archetypeName)
	if archetype == nil {
		return nil, fmt.Errorf("archetype %q not found in registry (available: %s)", archetypeName, strings.Join(archetypeNames(reg), ", "))
	}
	_, components, faults := activeSet(archetype, &types.ServiceDeclaration{Kind: "service", Archetype: archetypeName, Language: archetype.Language}, reg)
	if len(faults) > 0 {
		var messages []string
		for _, fault := range faults {
			messages = append(messages, fault.Message)
		}
		return nil, fmt.Errorf("archetype %q has no active component set: %s", archetypeName, strings.Join(messages, "; "))
	}
	return components, nil
}
