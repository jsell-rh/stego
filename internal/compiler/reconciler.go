package compiler

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

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
	ActionDelete    FileAction = "delete"
)

// PlannedFile represents a single file in the plan.
type PlannedFile struct {
	Path   string
	Action FileAction
}

// EntityChange describes a change to an entity's fields between applies.
type EntityChange struct {
	Entity   string
	Added    []string // "name (type)" format
	Removed  []string // "name (type)" format
	Modified []string // "name (old_type → new_type)" or "name (type)" format
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

// HasChanges returns true if the plan includes any generate, update, delete,
// or entity changes.
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

	// OutDir is the output directory for generated files. Defaults to
	// filepath.Join(ProjectDir, "out") if empty.
	OutDir string
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

	// Extract port binding overrides from service declaration.
	// String-valued entries in Overrides represent port→component bindings;
	// map-valued entries represent component config overrides (handled separately).
	servicePortOverrides := make(map[string]string)
	for key, val := range svcDecl.Overrides {
		if strVal, ok := val.(string); ok {
			servicePortOverrides[key] = strVal
		}
	}

	// Resolve ports.
	_, err = ports.Resolve(ports.ResolveInput{
		Components:        components,
		ArchetypeBindings: archetype.Bindings,
		ServiceOverrides:  servicePortOverrides,
	})
	if err != nil {
		return nil, fmt.Errorf("resolving ports: %w", err)
	}

	// Determine the slots package path. Slots are placed outside internal/
	// so that fills (which live at the project root, outside out/) can import
	// the generated slot interfaces.
	slotsPackage := ""
	if len(svcDecl.Slots) > 0 {
		slotsPackage = "slots"
	}

	// Resolve the HTTP port from the component that provides http-server,
	// using the component's config schema defaults merged with service overrides.
	httpPort := 8080
	for _, compName := range componentNames {
		comp := components[compName]
		providesHTTP := false
		for _, p := range comp.Provides {
			if p.Name == "http-server" {
				providesHTTP = true
				break
			}
		}
		if providesHTTP {
			config := resolveComponentConfig(comp, svcDecl)
			if portVal, ok := config["port"]; ok {
				switch v := portVal.(type) {
				case int:
					httpPort = v
				case float64:
					httpPort = int(v)
				}
			}
			break
		}
	}

	// Compute the output directory name relative to the project root.
	// go.mod is placed at the project root so that both generated packages
	// (under out/) and fill packages (under fills/) are within the module
	// root. Import paths for generated packages must include this prefix.
	outDir := input.OutDir
	if outDir == "" {
		outDir = filepath.Join(input.ProjectDir, "out")
	}
	outDirName, err := filepath.Rel(input.ProjectDir, outDir)
	if err != nil {
		outDirName = filepath.Base(outDir)
	}
	// Validate that outDirName is a proper subdirectory name. When OutDir
	// equals ProjectDir, filepath.Rel returns "." which produces invalid Go
	// import paths (e.g. "module/./internal/api"). Similarly, ".." would
	// escape the project root.
	if outDirName == "." || outDirName == ".." || strings.HasPrefix(outDirName, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("OutDir must be a subdirectory of ProjectDir: filepath.Rel(%q, %q) produced %q which is invalid in Go import paths", input.ProjectDir, outDir, outDirName)
	}

	// Resolve the auth package import path for generators that need to
	// extract caller identity from the request context (e.g. rest-api
	// populating Caller on slot requests via auth.IdentityFromContext).
	authPackage := ""
	for _, compName := range componentNames {
		comp := components[compName]
		for _, p := range comp.Provides {
			if p.Name == "auth-provider" && comp.OutputNamespace != "" {
				authPackage = input.ModuleName + "/" + outDirName + "/" + comp.OutputNamespace
				break
			}
		}
		if authPackage != "" {
			break
		}
	}

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
			Collections:     svcDecl.Collections,
			SlotBindings:    svcDecl.Slots,
			ModuleName:      input.ModuleName,
			SlotsPackage:    slotsPackage,
			ComponentConfig: resolveComponentConfig(comp, svcDecl),
			OutputNamespace: comp.OutputNamespace,
			OutDirName:      outDirName,
			AuthPackage:     authPackage,
		}

		files, wiring, err := generator.Generate(ctx)
		if err != nil {
			return nil, fmt.Errorf("generator %q: %w", compName, err)
		}

		// Validate namespace.
		if len(files) > 0 {
			if comp.OutputNamespace == "" {
				return nil, fmt.Errorf("generator %q produced %d file(s) but component declares no output_namespace — "+
					"all components that generate files must declare an output_namespace", compName, len(files))
			}
			if err := gen.ValidateNamespace(comp.OutputNamespace, files); err != nil {
				return nil, fmt.Errorf("generator %q: %w", compName, err)
			}
		}

		allFiles = append(allFiles, files...)
		wirings = append(wirings, ComponentWiring{Name: compName, Wiring: wiring})
	}

	// Generate slot interfaces and operators if slots are configured.
	slotFiles, err := generateSlotFiles(slotsPackage, input.RegistryDir, components, svcDecl, reg)
	if err != nil {
		return nil, fmt.Errorf("generating slot files: %w", err)
	}
	allFiles = append(allFiles, slotFiles...)

	// Validate that slot binding collections are in the collections list. Slot
	// operators are injected into handler constructors, which only exist for
	// collections. A slot binding referencing a non-existent collection would
	// produce an unused variable in the generated main.go — a Go compile error.
	if err := validateSlotCollectionsDefined(svcDecl.Slots, svcDecl.Collections); err != nil {
		return nil, err
	}

	// Assemble shared files (main.go, go.mod).
	assemblerInput := AssemblerInput{
		ModuleName:   input.ModuleName,
		ServiceName:  svcDecl.Name,
		GoVersion:    input.GoVersion,
		Port:         httpPort,
		Wirings:      wirings,
		SlotBindings: svcDecl.Slots,
		SlotsPackage: slotsPackage,
		OutDirName:   outDirName,
	}
	sharedFiles, err := Assemble(assemblerInput)
	if err != nil {
		return nil, fmt.Errorf("assembling shared files: %w", err)
	}
	allFiles = append(allFiles, sharedFiles...)

	// Validate no duplicate file paths across all sources (generators, slots,
	// assembler). A duplicate means one generator's output would silently
	// overwrite another's.
	if err := validateUniqueFilePaths(allFiles); err != nil {
		return nil, err
	}

	// Load existing state.
	statePath := filepath.Join(input.ProjectDir, ".stego", "state.yaml")
	existingState, err := LoadState(statePath)
	if err != nil {
		return nil, fmt.Errorf("loading state: %w", err)
	}

	// Compute plan by comparing generated files against existing state.
	plan := computePlan(allFiles, existingState, serviceData, svcDecl.Entities, components, outDir, input.ProjectDir, input.RegistrySHA)

	return plan, nil
}

// Apply writes all generated files to disk under outDir (or projectDir for
// project-root files like go.mod), removes orphaned files, and saves the
// new state.
func Apply(plan *Plan, projectDir, outDir string) error {
	// Write generated files.
	for _, f := range plan.GeneratedFiles {
		baseDir := fileBaseDir(f.Path, outDir, projectDir)
		fullPath := filepath.Join(baseDir, f.Path)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			return fmt.Errorf("creating directory for %s: %w", f.Path, err)
		}
		content := f.Bytes()
		if err := os.WriteFile(fullPath, content, 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", f.Path, err)
		}
	}

	// Remove orphaned files (tracked in previous state but no longer generated).
	for _, pf := range plan.Files {
		if pf.Action == ActionDelete {
			baseDir := fileBaseDir(pf.Path, outDir, projectDir)
			fullPath := filepath.Join(baseDir, pf.Path)
			if err := os.Remove(fullPath); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("removing orphaned file %s: %w", pf.Path, err)
			}
		}
	}

	statePath := filepath.Join(projectDir, ".stego", "state.yaml")
	return SaveState(statePath, plan.NewState)
}

// fileBaseDir returns the base directory for a file path. Project-root files
// (go.mod) are placed at projectDir; all other generated files go to outDir.
func fileBaseDir(filePath, outDir, projectDir string) string {
	if isProjectRootFile(filePath) {
		return projectDir
	}
	return outDir
}

// isProjectRootFile returns true for files that should be placed at the project
// root rather than in the output directory. Currently only go.mod, because it
// must be at the module root to make both generated packages (under out/) and
// fill packages (under fills/) resolvable as intra-module imports.
func isProjectRootFile(filePath string) bool {
	return filePath == "go.mod"
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

	// Validate declared mixins against archetype's compatible_mixins constraint.
	// Distinguish nil (no field declared — any mixin accepted) from empty
	// (compatible_mixins: [] — no mixins are compatible).
	if archetype.CompatibleMixins != nil && len(svcDecl.Mixins) > 0 {
		compatible := make(map[string]bool, len(archetype.CompatibleMixins))
		for _, m := range archetype.CompatibleMixins {
			compatible[m] = true
		}
		for _, mixinName := range svcDecl.Mixins {
			if !compatible[mixinName] {
				return nil, fmt.Errorf("mixin %q is not compatible with archetype %q (compatible mixins: %v)", mixinName, archetype.Name, archetype.CompatibleMixins)
			}
		}
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
func generateSlotFiles(slotsPackage, registryDir string, components map[string]*types.Component, svcDecl *types.ServiceDeclaration, reg *registry.Registry) ([]gen.File, error) {
	if slotsPackage == "" || len(svcDecl.Slots) == 0 {
		return nil, nil
	}

	pkgName := filepath.Base(slotsPackage)

	// Collect unique slot names from bindings.
	slotNames := make(map[string]bool)
	for _, sb := range svcDecl.Slots {
		slotNames[sb.Slot] = true
	}

	// Find the slot definitions and their owning components or mixins.
	type slotInfo struct {
		definition types.SlotDefinition
		// protoDir is the directory containing the slots/ subdirectory for this
		// slot's proto file. For component slots: <registry>/components/<name>.
		// For mixin-added slots: <registry>/mixins/<name>.
		protoDir string
	}
	slotDefs := make(map[string]slotInfo)
	for _, comp := range components {
		for _, sd := range comp.Slots {
			if slotNames[sd.Name] {
				slotDefs[sd.Name] = slotInfo{
					definition: sd,
					protoDir:   filepath.Join(registryDir, "components", comp.Name),
				}
			}
		}
	}
	// Also search mixin adds_slots for slot definitions not found in components.
	for _, mixinName := range svcDecl.Mixins {
		mixin := reg.Mixin(mixinName)
		if mixin == nil {
			continue
		}
		for _, sd := range mixin.AddsSlots {
			if slotNames[sd.Name] {
				if _, exists := slotDefs[sd.Name]; !exists {
					slotDefs[sd.Name] = slotInfo{
						definition: sd,
						protoDir:   filepath.Join(registryDir, "mixins", mixin.Name),
					}
				}
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

	// Track types emitted across all slot files to avoid duplicate
	// declarations when multiple slots share imported types (e.g. SlotResult).
	emittedTypes := make(map[string]bool)

	for _, slotName := range sortedSlots {
		info, ok := slotDefs[slotName]
		if !ok {
			return nil, fmt.Errorf("slot %q not defined by any component or mixin", slotName)
		}

		protoPath := filepath.Join(info.protoDir, "slots", slotName+".proto")

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

		// Generate interface file, excluding types already emitted by a
		// previous slot file in the same package.
		ifaceFile, err := slot.GenerateInterfaceExcluding(
			filepath.Join(slotsPackage, slotName+".go"),
			pkgName,
			parsed,
			imports,
			emittedTypes,
		)
		if err != nil {
			return nil, fmt.Errorf("generating interface for slot %q: %w", slotName, err)
		}
		files = append(files, ifaceFile)

		// Record all types that were referenced (and thus emitted) by this
		// slot file so subsequent files skip them.
		allMessages := make(map[string]slot.Message)
		for _, imp := range imports {
			for _, msg := range imp.Messages {
				allMessages[imp.Package+"."+msg.Name] = msg
				allMessages[msg.Name] = msg
			}
		}
		for _, msg := range parsed.Messages {
			allMessages[parsed.Package+"."+msg.Name] = msg
			allMessages[msg.Name] = msg
		}
		for typeName := range slot.CollectAllReferencedTypes(parsed, allMessages) {
			emittedTypes[typeName] = true
		}

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
	outDir string,
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

		baseDir := fileBaseDir(f.Path, outDir, projectDir)
		existingHash, exists := existingHashes[f.Path]
		switch {
		case !exists:
			diskPath := filepath.Join(baseDir, f.Path)
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
			// State hash matches generated hash, but verify the file still
			// exists on disk. A manually deleted file should be regenerated,
			// not silently omitted from the plan.
			diskPath := filepath.Join(baseDir, f.Path)
			if _, err := os.Stat(diskPath); os.IsNotExist(err) {
				planned = append(planned, PlannedFile{Path: f.Path, Action: ActionGenerate})
			} else {
				planned = append(planned, PlannedFile{Path: f.Path, Action: ActionUnchanged})
			}
		default:
			planned = append(planned, PlannedFile{Path: f.Path, Action: ActionUpdate})
		}
	}

	// Detect orphaned files: tracked in previous state but no longer generated.
	for path := range existingHashes {
		if _, stillGenerated := newFileHashes[path]; !stillGenerated {
			planned = append(planned, PlannedFile{Path: path, Action: ActionDelete})
		}
	}

	sort.Slice(planned, func(i, j int) bool {
		return planned[i].Path < planned[j].Path
	})

	// Compute entity field changes.
	entityChanges := computeEntityChanges(entities, existingState)

	// Build new entity snapshot for state, capturing field types and hashes
	// for change detection.
	entitySnapshot := make(map[string][]EntityFieldState, len(entities))
	for _, e := range entities {
		var fields []EntityFieldState
		for _, f := range e.Fields {
			fields = append(fields, EntityFieldState{
				Name: f.Name,
				Type: string(f.Type),
				Hash: fieldHash(f),
			})
		}
		entitySnapshot[e.Name] = fields
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

// fieldDescriptor formats a field name and type for plan display.
func fieldDescriptor(name, typ string) string {
	return fmt.Sprintf("%s (%s)", name, typ)
}

// fieldHash computes a SHA-256 hash of a serialized field definition,
// capturing type, constraints, and all attributes for change detection.
func fieldHash(f types.Field) string {
	data, _ := yaml.Marshal(f)
	return HashBytes(data)
}

// computeEntityChanges diffs current entities against the previous apply's
// entity snapshot to find added, removed, and modified fields.
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

	// Detect additions, modifications, and field removals in current entities.
	for _, e := range entities {
		oldFields, existed := oldEntities[e.Name]
		if !existed {
			// Entire entity is new — all fields are additions.
			var added []string
			for _, f := range e.Fields {
				added = append(added, fieldDescriptor(f.Name, string(f.Type)))
			}
			if len(added) > 0 {
				changes = append(changes, EntityChange{
					Entity: e.Name,
					Added:  added,
				})
			}
			continue
		}

		// Build lookup from old field name to its snapshot.
		oldByName := make(map[string]EntityFieldState, len(oldFields))
		for _, f := range oldFields {
			oldByName[f.Name] = f
		}

		newSet := make(map[string]bool)
		var added, modified []string
		for _, f := range e.Fields {
			newSet[f.Name] = true
			oldField, wasPresent := oldByName[f.Name]
			if !wasPresent {
				added = append(added, fieldDescriptor(f.Name, string(f.Type)))
				continue
			}
			// Field exists in both — check for type or constraint changes via hash.
			newHash := fieldHash(f)
			if newHash != oldField.Hash {
				if string(f.Type) != oldField.Type {
					modified = append(modified, fieldDescriptor(f.Name, oldField.Type+" → "+string(f.Type)))
				} else {
					modified = append(modified, fieldDescriptor(f.Name, string(f.Type)))
				}
			}
		}

		var removed []string
		for _, f := range oldFields {
			if !newSet[f.Name] {
				removed = append(removed, fieldDescriptor(f.Name, f.Type))
			}
		}

		if len(added) > 0 || len(removed) > 0 || len(modified) > 0 {
			changes = append(changes, EntityChange{
				Entity:   e.Name,
				Added:    added,
				Removed:  removed,
				Modified: modified,
			})
		}
	}

	// Detect deleted entities: present in old state but absent from current.
	// Collect and sort deleted entity names for deterministic output.
	var deletedNames []string
	for entityName := range oldEntities {
		if !currentNames[entityName] {
			deletedNames = append(deletedNames, entityName)
		}
	}
	sort.Strings(deletedNames)
	for _, entityName := range deletedNames {
		oldFields := oldEntities[entityName]
		var removed []string
		for _, f := range oldFields {
			removed = append(removed, fieldDescriptor(f.Name, f.Type))
		}
		changes = append(changes, EntityChange{
			Entity:  entityName,
			Removed: removed,
		})
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
			for _, f := range ec.Modified {
				fmt.Fprintf(&result, "    ~ field: %s\n", f)
			}
			for _, f := range ec.Removed {
				fmt.Fprintf(&result, "    - field: %s\n", f)
			}
		}
		result.WriteString("\n")
	}

	var sb strings.Builder
	var generateCount, updateCount, deleteCount, unchangedCount int

	for _, f := range plan.Files {
		switch f.Action {
		case ActionGenerate:
			generateCount++
			fmt.Fprintf(&sb, "  generate: %s\n", f.Path)
		case ActionUpdate:
			updateCount++
			fmt.Fprintf(&sb, "  update:   %s\n", f.Path)
		case ActionDelete:
			deleteCount++
			fmt.Fprintf(&sb, "  delete:   %s\n", f.Path)
		case ActionUnchanged:
			unchangedCount++
		}
	}

	result.WriteString("Plan:\n")
	result.WriteString(sb.String())
	if unchangedCount > 0 {
		fmt.Fprintf(&result, "  unchanged: %d files\n", unchangedCount)
	}
	fmt.Fprintf(&result, "\nSummary: %d to generate, %d to update, %d to delete, %d unchanged\n",
		generateCount, updateCount, deleteCount, unchangedCount)

	return result.String()
}

// validateSlotCollectionsDefined checks that every slot binding with a non-empty
// Collection field references a collection that exists. Slot operators are
// injected into handler constructors, which are only generated for collections.
// A binding referencing a non-existent collection would produce an unused
// variable — a Go compile error.
func validateSlotCollectionsDefined(slots []types.SlotDeclaration, collections []types.Collection) error {
	if len(slots) == 0 {
		return nil
	}

	// Build set of collection names.
	collectionNames := make(map[string]bool, len(collections))
	for _, c := range collections {
		collectionNames[c.Name] = true
	}

	for _, sb := range slots {
		if sb.Collection == "" {
			continue
		}
		if !collectionNames[sb.Collection] {
			var names []string
			for _, c := range collections {
				names = append(names, c.Name)
			}
			return fmt.Errorf("slot binding %q references collection %q but %q is not defined — "+
				"slot operators can only be wired to defined collections (available: %v)",
				sb.Slot, sb.Collection, sb.Collection, names)
		}
	}
	return nil
}

// validateUniqueFilePaths checks that no two files in the collection share the
// same output path. Duplicate paths mean one source's output would silently
// overwrite another's.
func validateUniqueFilePaths(files []gen.File) error {
	seen := make(map[string]bool, len(files))
	var duplicates []string
	for _, f := range files {
		if seen[f.Path] {
			duplicates = append(duplicates, f.Path)
		}
		seen[f.Path] = true
	}
	if len(duplicates) > 0 {
		sort.Strings(duplicates)
		return fmt.Errorf("duplicate generated file paths: %s", strings.Join(duplicates, ", "))
	}
	return nil
}
