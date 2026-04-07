package compiler

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/parser"
	"github.com/jsell-rh/stego/internal/ports"
	"github.com/jsell-rh/stego/internal/registry"
	"github.com/jsell-rh/stego/internal/slot"
	"github.com/jsell-rh/stego/internal/types"
)

// FileAction describes what will happen to a generated file.
type FileAction string

const (
	ActionGenerate  FileAction = "generate"
	ActionUpdate    FileAction = "update"
	ActionUnchanged FileAction = "unchanged"
)

// PlannedFile represents a single file in the plan.
type PlannedFile struct {
	Path   string
	Action FileAction
}

// EntityChange describes a change to an entity's fields.
type EntityChange struct {
	Entity string
	Added  []string
	Removed []string
}

// Plan represents the computed changeset between desired and current state.
type Plan struct {
	// Files lists every file that would be generated, along with its action.
	Files []PlannedFile

	// EntityChanges lists entity field changes detected between the current
	// service.yaml and the previous apply.
	EntityChanges []EntityChange

	// GeneratedFiles holds the actual file contents ready to write.
	GeneratedFiles []gen.File

	// NewState is the state that will be written after a successful apply.
	NewState *State
}

// HasChanges returns true if the plan includes any generate, update, or
// entity changes.
func (p *Plan) HasChanges() bool {
	if len(p.EntityChanges) > 0 {
		return true
	}
	for _, f := range p.Files {
		if f.Action != ActionUnchanged {
			return true
		}
	}
	return false
}

// ReconcilerInput gathers everything needed to compute a plan.
type ReconcilerInput struct {
	// ProjectDir is the root of the project (where service.yaml lives).
	ProjectDir string

	// RegistryDir is the path to the registry directory.
	RegistryDir string

	// Generators maps component name to its Generator implementation.
	Generators map[string]gen.Generator

	// GoVersion is the Go version for go.mod (e.g. "1.22").
	GoVersion string

	// ModuleName is the Go module path (e.g. "github.com/myorg/user-service").
	ModuleName string

	// RegistrySHA is the registry ref SHA from .stego/config.yaml, used for
	// auditability in state tracking.
	RegistrySHA string
}

// Reconcile computes a plan by loading the service declaration, resolving the
// archetype, running all component generators, and assembling shared files.
// The plan can then be inspected (plan) or applied (Apply).
func Reconcile(input ReconcilerInput) (*Plan, error) {
	// Load service declaration. Read once and parse from bytes to avoid
	// TOCTOU race between hashing and parsing.
	serviceYAMLPath := filepath.Join(input.ProjectDir, "service.yaml")
	serviceData, err := os.ReadFile(serviceYAMLPath)
	if err != nil {
		return nil, fmt.Errorf("reading service.yaml: %w", err)
	}
	svcDecl, err := parser.ParseServiceDeclarationFromBytes(serviceData, serviceYAMLPath)
	if err != nil {
		return nil, fmt.Errorf("parsing service.yaml: %w", err)
	}

	// Load registry.
	reg, err := registry.Load(input.RegistryDir)
	if err != nil {
		return nil, fmt.Errorf("loading registry: %w", err)
	}

	// Resolve archetype.
	archetype := reg.Archetype(svcDecl.Archetype)
	if archetype == nil {
		return nil, fmt.Errorf("archetype %q not found in registry", svcDecl.Archetype)
	}

	// Collect all component names: archetype components + default_auth + mixin components.
	componentNames, err := collectComponentNames(archetype, svcDecl, reg)
	if err != nil {
		return nil, err
	}

	// Look up all components from registry.
	components := make(map[string]*types.Component)
	for _, name := range componentNames {
		comp := reg.Component(name)
		if comp == nil {
			return nil, fmt.Errorf("component %q not found in registry (referenced by archetype %q)", name, archetype.Name)
		}
		components[name] = comp
	}

	// Resolve ports.
	_, err = ports.Resolve(ports.ResolveInput{
		Components:        components,
		ArchetypeBindings: archetype.Bindings,
	})
	if err != nil {
		return nil, fmt.Errorf("resolving ports: %w", err)
	}

	// Determine the slots package path.
	slotsPackage := ""
	if len(svcDecl.Slots) > 0 {
		slotsPackage = "internal/slots"
	}

	// Get the HTTP port from component config or defaults.
	httpPort := 8080

	// Run all component generators in archetype-declared order.
	var allFiles []gen.File
	var wirings []ComponentWiring

	for _, compName := range componentNames {
		comp := components[compName]
		generator, ok := input.Generators[compName]
		if !ok {
			// No generator registered — skip with nil wiring.
			wirings = append(wirings, ComponentWiring{Name: compName, Wiring: nil})
			continue
		}

		ctx := gen.Context{
			Conventions:     archetype.Conventions,
			Entities:        svcDecl.Entities,
			Expose:          svcDecl.Expose,
			SlotBindings:    svcDecl.Slots,
			ModuleName:      input.ModuleName,
			SlotsPackage:    slotsPackage,
			ComponentConfig: resolveComponentConfig(comp, svcDecl),
			OutputNamespace: comp.OutputNamespace,
		}

		files, wiring, err := generator.Generate(ctx)
		if err != nil {
			return nil, fmt.Errorf("generator %q: %w", compName, err)
		}

		// Validate namespace.
		if comp.OutputNamespace != "" && len(files) > 0 {
			if err := gen.ValidateNamespace(comp.OutputNamespace, files); err != nil {
				return nil, fmt.Errorf("generator %q: %w", compName, err)
			}
		}

		allFiles = append(allFiles, files...)
		wirings = append(wirings, ComponentWiring{Name: compName, Wiring: wiring})
	}

	// Generate slot interfaces and operators if slots are configured.
	slotFiles, err := generateSlotFiles(slotsPackage, input.RegistryDir, components, svcDecl)
	if err != nil {
		return nil, fmt.Errorf("generating slot files: %w", err)
	}
	allFiles = append(allFiles, slotFiles...)

	// Assemble shared files (main.go, go.mod).
	assemblerInput := AssemblerInput{
		ModuleName:   input.ModuleName,
		ServiceName:  svcDecl.Name,
		GoVersion:    input.GoVersion,
		Port:         httpPort,
		Wirings:      wirings,
		SlotBindings: svcDecl.Slots,
		SlotsPackage: slotsPackage,
	}
	sharedFiles, err := Assemble(assemblerInput)
	if err != nil {
		return nil, fmt.Errorf("assembling shared files: %w", err)
	}
	allFiles = append(allFiles, sharedFiles...)

	// Load existing state.
	statePath := filepath.Join(input.ProjectDir, ".stego", "state.yaml")
	existingState, err := LoadState(statePath)
	if err != nil {
		return nil, fmt.Errorf("loading state: %w", err)
	}

	// Compute plan by comparing generated files against existing state.
	plan := computePlan(allFiles, existingState, serviceData, svcDecl.Entities, components, input.ProjectDir, input.RegistrySHA)

	return plan, nil
}

// Apply writes all generated files to disk under outDir and saves the new state.
func Apply(plan *Plan, projectDir, outDir string) error {
	for _, f := range plan.GeneratedFiles {
		fullPath := filepath.Join(outDir, f.Path)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			return fmt.Errorf("creating directory for %s: %w", f.Path, err)
		}
		content := f.Bytes()
		if err := os.WriteFile(fullPath, content, 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", f.Path, err)
		}
	}

	statePath := filepath.Join(projectDir, ".stego", "state.yaml")
	return SaveState(statePath, plan.NewState)
}

// collectComponentNames assembles the ordered list of component names from the
// archetype, default_auth, and any mixin-added components.
func collectComponentNames(archetype *types.Archetype, svcDecl *types.ServiceDeclaration, reg *registry.Registry) ([]string, error) {
	seen := make(map[string]bool)
	var names []string

	for _, name := range archetype.Components {
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}

	if archetype.DefaultAuth != "" && !seen[archetype.DefaultAuth] {
		seen[archetype.DefaultAuth] = true
		names = append(names, archetype.DefaultAuth)
	}

	for _, mixinName := range svcDecl.Mixins {
		mixin := reg.Mixin(mixinName)
		if mixin == nil {
			return nil, fmt.Errorf("mixin %q not found in registry", mixinName)
		}
		for _, compName := range mixin.AddsComponents {
			if !seen[compName] {
				seen[compName] = true
				names = append(names, compName)
			}
		}
	}

	return names, nil
}

// resolveComponentConfig merges a component's default config values with any
// service-level overrides for that component.
func resolveComponentConfig(comp *types.Component, svcDecl *types.ServiceDeclaration) map[string]any {
	config := make(map[string]any)

	for key, field := range comp.Config {
		if field.Default != nil {
			config[key] = field.Default
		}
	}

	if overrides, ok := svcDecl.Overrides[comp.Name]; ok {
		if m, ok := overrides.(map[string]any); ok {
			for k, v := range m {
				config[k] = v
			}
		}
	}

	return config
}

// generateSlotFiles generates Go interface and operator files for all slots
// used by the service declaration.
func generateSlotFiles(slotsPackage, registryDir string, components map[string]*types.Component, svcDecl *types.ServiceDeclaration) ([]gen.File, error) {
	if slotsPackage == "" || len(svcDecl.Slots) == 0 {
		return nil, nil
	}

	pkgName := filepath.Base(slotsPackage)

	// Collect unique slot names from bindings.
	slotNames := make(map[string]bool)
	for _, sb := range svcDecl.Slots {
		slotNames[sb.Slot] = true
	}

	// Find the slot definitions and their owning components.
	type slotInfo struct {
		definition types.SlotDefinition
		component  string
	}
	slotDefs := make(map[string]slotInfo)
	for _, comp := range components {
		for _, sd := range comp.Slots {
			if slotNames[sd.Name] {
				slotDefs[sd.Name] = slotInfo{definition: sd, component: comp.Name}
			}
		}
	}

	// Sort slot names for deterministic output.
	sortedSlots := make([]string, 0, len(slotNames))
	for name := range slotNames {
		sortedSlots = append(sortedSlots, name)
	}
	sort.Strings(sortedSlots)

	var files []gen.File

	for _, slotName := range sortedSlots {
		info, ok := slotDefs[slotName]
		if !ok {
			return nil, fmt.Errorf("slot %q not defined by any component", slotName)
		}

		// Proto file lives at <registry>/components/<component>/slots/<slot>.proto.
		protoPath := filepath.Join(registryDir, "components", info.component, "slots", slotName+".proto")

		protoFile, err := os.Open(protoPath)
		if err != nil {
			return nil, fmt.Errorf("opening proto for slot %q: %w", slotName, err)
		}
		parsed, err := slot.ParseProto(protoFile)
		protoFile.Close()
		if err != nil {
			return nil, fmt.Errorf("parsing proto for slot %q: %w", slotName, err)
		}

		// Resolve proto imports (e.g. stego/common/types.proto).
		var imports []*slot.ProtoFile
		for _, imp := range parsed.Imports {
			impPath := resolveProtoImport(imp, registryDir)
			if impPath == "" {
				continue
			}
			impFile, err := os.Open(impPath)
			if err != nil {
				continue
			}
			impParsed, err := slot.ParseProto(impFile)
			impFile.Close()
			if err != nil {
				continue
			}
			imports = append(imports, impParsed)
		}

		// Generate interface file.
		ifaceFile, err := slot.GenerateInterface(
			filepath.Join(slotsPackage, slotName+".go"),
			pkgName,
			parsed,
			imports,
		)
		if err != nil {
			return nil, fmt.Errorf("generating interface for slot %q: %w", slotName, err)
		}
		files = append(files, ifaceFile)

		// Generate operators file.
		opsFile, err := slot.GenerateOperators(
			filepath.Join(slotsPackage, slotName+"_operators.go"),
			pkgName,
			parsed,
		)
		if err != nil {
			return nil, fmt.Errorf("generating operators for slot %q: %w", slotName, err)
		}
		files = append(files, opsFile)
	}

	return files, nil
}

// resolveProtoImport maps a proto import path to a file on disk.
// Proto imports like "stego/common/types.proto" map to <registry>/common/types.proto.
func resolveProtoImport(importPath, registryDir string) string {
	// Strip the "stego/" prefix that proto packages use.
	trimmed := strings.TrimPrefix(importPath, "stego/")
	candidate := filepath.Join(registryDir, trimmed)
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}
	// Also try the import path directly.
	candidate = filepath.Join(registryDir, importPath)
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}
	return ""
}

// computePlan compares generated files against existing state to determine
// which files need to be written and what entity changes occurred.
func computePlan(
	generatedFiles []gen.File,
	existingState *State,
	serviceData []byte,
	entities []types.Entity,
	components map[string]*types.Component,
	projectDir string,
	registrySHA string,
) *Plan {
	serviceHash := HashBytes(serviceData)

	existingHashes := make(map[string]string)
	if existingState.LastApplied != nil && existingState.LastApplied.Files != nil {
		existingHashes = existingState.LastApplied.Files
	}

	var planned []PlannedFile
	newFileHashes := make(map[string]string)

	for _, f := range generatedFiles {
		content := f.Bytes()
		hash := HashBytes(content)
		newFileHashes[f.Path] = hash

		existingHash, exists := existingHashes[f.Path]
		switch {
		case !exists:
			diskPath := filepath.Join(projectDir, "out", f.Path)
			if _, err := os.Stat(diskPath); os.IsNotExist(err) {
				planned = append(planned, PlannedFile{Path: f.Path, Action: ActionGenerate})
			} else {
				diskData, err := os.ReadFile(diskPath)
				if err == nil && HashBytes(diskData) == hash {
					planned = append(planned, PlannedFile{Path: f.Path, Action: ActionUnchanged})
				} else {
					planned = append(planned, PlannedFile{Path: f.Path, Action: ActionUpdate})
				}
			}
		case existingHash == hash:
			planned = append(planned, PlannedFile{Path: f.Path, Action: ActionUnchanged})
		default:
			planned = append(planned, PlannedFile{Path: f.Path, Action: ActionUpdate})
		}
	}

	sort.Slice(planned, func(i, j int) bool {
		return planned[i].Path < planned[j].Path
	})

	// Compute entity field changes.
	entityChanges := computeEntityChanges(entities, existingState)

	// Build new entity snapshot for state.
	entitySnapshot := make(map[string][]string, len(entities))
	for _, e := range entities {
		var fieldNames []string
		for _, f := range e.Fields {
			fieldNames = append(fieldNames, f.Name)
		}
		entitySnapshot[e.Name] = fieldNames
	}

	compState := make(map[string]ComponentState)
	for name, comp := range components {
		compState[name] = ComponentState{
			Version: comp.Version,
			SHA:     registrySHA,
		}
	}

	newState := &State{
		LastApplied: &AppliedState{
			ServiceHash: serviceHash,
			RegistrySHA: registrySHA,
			Components:  compState,
			Entities:    entitySnapshot,
			Files:       newFileHashes,
		},
	}

	return &Plan{
		Files:          planned,
		EntityChanges:  entityChanges,
		GeneratedFiles: generatedFiles,
		NewState:       newState,
	}
}

// computeEntityChanges diffs current entities against the previous apply's
// entity snapshot to find added and removed fields.
func computeEntityChanges(entities []types.Entity, existingState *State) []EntityChange {
	if existingState.LastApplied == nil || existingState.LastApplied.Entities == nil {
		return nil
	}

	oldEntities := existingState.LastApplied.Entities

	// Build a set of current entity names for deletion detection.
	currentNames := make(map[string]bool, len(entities))
	for _, e := range entities {
		currentNames[e.Name] = true
	}

	var changes []EntityChange

	// Detect additions and modifications in current entities.
	for _, e := range entities {
		oldFields, existed := oldEntities[e.Name]
		if !existed {
			// Entire entity is new — all fields are additions.
			var added []string
			for _, f := range e.Fields {
				added = append(added, f.Name)
			}
			if len(added) > 0 {
				changes = append(changes, EntityChange{
					Entity: e.Name,
					Added:  added,
				})
			}
			continue
		}

		oldSet := make(map[string]bool, len(oldFields))
		for _, f := range oldFields {
			oldSet[f] = true
		}

		newSet := make(map[string]bool)
		var added []string
		for _, f := range e.Fields {
			newSet[f.Name] = true
			if !oldSet[f.Name] {
				added = append(added, f.Name)
			}
		}

		var removed []string
		for _, f := range oldFields {
			if !newSet[f] {
				removed = append(removed, f)
			}
		}

		if len(added) > 0 || len(removed) > 0 {
			changes = append(changes, EntityChange{
				Entity:  e.Name,
				Added:   added,
				Removed: removed,
			})
		}
	}

	// Detect deleted entities: present in old state but absent from current.
	for entityName, oldFields := range oldEntities {
		if !currentNames[entityName] {
			changes = append(changes, EntityChange{
				Entity:  entityName,
				Removed: oldFields,
			})
		}
	}

	return changes
}

// FormatPlan produces a human-readable summary of the plan.
func FormatPlan(plan *Plan) string {
	if !plan.HasChanges() {
		return "No changes. Infrastructure is up-to-date."
	}

	var result strings.Builder

	// Show entity field changes if any.
	if len(plan.EntityChanges) > 0 {
		result.WriteString("Changes detected in service.yaml:\n")
		for _, ec := range plan.EntityChanges {
			fmt.Fprintf(&result, "  entities.%s:\n", ec.Entity)
			for _, f := range ec.Added {
				fmt.Fprintf(&result, "    + field: %s\n", f)
			}
			for _, f := range ec.Removed {
				fmt.Fprintf(&result, "    - field: %s\n", f)
			}
		}
		result.WriteString("\n")
	}

	var sb strings.Builder
	var generateCount, updateCount, unchangedCount int

	for _, f := range plan.Files {
		switch f.Action {
		case ActionGenerate:
			generateCount++
			fmt.Fprintf(&sb, "  generate: %s\n", f.Path)
		case ActionUpdate:
			updateCount++
			fmt.Fprintf(&sb, "  update:   %s\n", f.Path)
		case ActionUnchanged:
			unchangedCount++
		}
	}

	result.WriteString("Plan:\n")
	result.WriteString(sb.String())
	if unchangedCount > 0 {
		fmt.Fprintf(&result, "  unchanged: %d files\n", unchangedCount)
	}
	fmt.Fprintf(&result, "\nSummary: %d to generate, %d to update, %d unchanged\n",
		generateCount, updateCount, unchangedCount)

	return result.String()
}
