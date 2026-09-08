package compiler

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/jsell-rh/stego/internal/gen"
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

	// StateChanged includes input and ownership changes with unchanged output.
	StateChanged bool

	snapshots  map[string]fileSnapshot
	projectDir string
	outDir     string
}

// HasChanges returns true if the plan includes any generate, update, delete,
// or entity changes.
func (p *Plan) HasChanges() bool {
	if p.StateChanged {
		return true
	}
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
	if err := checkPendingProject(input.ProjectDir); err != nil {
		return nil, err
	}
	source, err := loadCompilationSource(input)
	if err != nil {
		return nil, err
	}
	validation, err := validateSource(input, source)
	if err != nil {
		return nil, err
	}
	if validation.HasErrors() {
		return nil, fmt.Errorf("service validation failed:\n%s", FormatValidation(validation))
	}
	serviceData, svcDecl, reg := source.ServiceData, source.Service, source.Registry
	existingState, inputSnapshots, err := captureProjectInputs(input.ProjectDir, serviceData)
	if err != nil {
		return nil, err
	}
	archetype := reg.Archetype(svcDecl.Archetype)

	// Collect baseline component names: archetype components + default_auth + mixin components.
	baselineNames, err := collectComponentNames(archetype, svcDecl, reg)
	if err != nil {
		return nil, err
	}

	// Look up baseline components from registry.
	baselineComponents := make(map[string]*types.Component)
	for _, name := range baselineNames {
		comp := reg.Component(name)
		if comp == nil {
			return nil, fmt.Errorf("component %q not found in registry (referenced by archetype %q)", name, archetype.Name)
		}
		baselineComponents[name] = comp
	}

	// Apply convention overrides from the service declaration (e.g.
	// "cors: disabled" suppresses CORS middleware generation even when the
	// archetype defaults to "cors: enabled").
	conventions := applyConventionOverrides(archetype.Conventions, svcDecl.Overrides)

	// Extract port binding overrides from service declaration.
	// String-valued entries in Overrides represent port→component bindings;
	// map-valued entries represent component config overrides (handled separately).
	// Convention override keys (e.g. "cors") are excluded from port resolution.
	servicePortOverrides := make(map[string]string)
	for key, val := range svcDecl.Overrides {
		if strVal, ok := val.(string); ok {
			if isConventionOverrideKey(key) {
				continue
			}
			servicePortOverrides[key] = strVal
		}
	}

	// Resolve ports. The resolver loads override components from the registry
	// and excludes replaced archetype defaults from the active component set.
	resolution, err := ports.Resolve(ports.ResolveInput{
		Components:        baselineComponents,
		ArchetypeBindings: archetype.Bindings,
		ServiceOverrides:  servicePortOverrides,
		ComponentLoader:   reg.Component,
	})
	if err != nil {
		return nil, fmt.Errorf("resolving ports: %w", err)
	}

	// Use the resolver's active component set (post-override) for all
	// downstream processing. Rebuild an ordered component name list:
	// preserve baseline order (filtering out replaced defaults), then
	// append newly loaded override components.
	components := resolution.ActiveComponents
	componentNames := make([]string, 0, len(components))
	for _, name := range baselineNames {
		if _, ok := components[name]; ok {
			componentNames = append(componentNames, name)
		}
	}
	// Append override components that were loaded by the resolver and are
	// not already in the ordered list (i.e. they were not in the baseline).
	// Sort for deterministic ordering.
	baselineSet := make(map[string]bool, len(baselineNames))
	for _, name := range baselineNames {
		baselineSet[name] = true
	}
	var newOverrides []string
	for name := range components {
		if !baselineSet[name] {
			newOverrides = append(newOverrides, name)
		}
	}
	sort.Strings(newOverrides)
	componentNames = append(componentNames, newOverrides...)

	// Determine the slots package path. Slots are placed outside internal/
	// so that fills (which live at the project root, outside out/) can import
	// the generated slot interfaces.
	slotsPackage := ""
	if len(svcDecl.Slots) > 0 {
		slotsPackage = "slots"
	}

	// Compute the output directory name relative to the project root.
	// go.mod is placed at the project root so that both generated packages
	// (under out/) and fill packages (under fills/) are within the module
	// root. Import paths for generated packages must include this prefix.
	outDir := input.OutDir
	if outDir == "" {
		outDir = filepath.Join(input.ProjectDir, "out")
	}
	outDirName, err := outputRelative(input.ProjectDir, outDir)
	if err != nil {
		return nil, err
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

	// Build peer namespace map so generators can reference types from other
	// components (e.g. storage adapter imports API package's ListOptions).
	peerNamespaces := make(map[string]string, len(componentNames))
	for _, compName := range componentNames {
		comp := components[compName]
		if comp.OutputNamespace != "" {
			peerNamespaces[compName] = comp.OutputNamespace
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
			Conventions:     conventions,
			Entities:        svcDecl.Entities,
			Collections:     svcDecl.Collections,
			SlotBindings:    svcDecl.Slots,
			ModuleName:      input.ModuleName,
			SlotsPackage:    slotsPackage,
			ComponentConfig: resolveComponentConfig(comp, svcDecl),
			OutputNamespace: comp.OutputNamespace,
			OutDirName:      outDirName,
			AuthPackage:     authPackage,
			BasePath:        svcDecl.BasePath,
			ServiceName:     svcDecl.Name,
			ErrorTypeBase:   svcDecl.ErrorTypeBase,
			PeerNamespaces:  peerNamespaces,
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
		Wirings:      wirings,
		SlotBindings: svcDecl.Slots,
		SlotsPackage: slotsPackage,
		OutDirName:   outDirName,
	}
	sharedFiles, err := Assemble(assemblerInput)
	if err != nil {
		return nil, fmt.Errorf("assembling shared files: %w", err)
	}
	for i, file := range sharedFiles {
		if file.Path == "go.mod" {
			sharedFiles[i], err = mergeProjectModule(input.ProjectDir, file)
			if err != nil {
				return nil, err
			}
		}
	}
	allFiles = append(allFiles, sharedFiles...)

	// Validate no duplicate file paths across all sources (generators, slots,
	// assembler). A duplicate means one generator's output would silently
	// overwrite another's.
	if err := validateUniqueFilePaths(allFiles); err != nil {
		return nil, err
	}

	// Compute plan by comparing generated files against existing state.
	plan, err := computePlan(allFiles, existingState, serviceData, svcDecl.Entities, components, outDir, input.ProjectDir, input.RegistrySHA)
	if err != nil {
		return nil, err
	}
	if err := bindPlanInputs(plan, input.ProjectDir, inputSnapshots); err != nil {
		return nil, err
	}
	return plan, nil
}

// Apply writes all generated files to disk under outDir (or projectDir for
// project-root files like go.mod), removes orphaned files, and saves the
// new state.
func Apply(plan *Plan, projectDir, outDir string) error {
	return applyWithFault(plan, projectDir, outDir, nil)
}

func applyWithFault(plan *Plan, projectDir, outDir string, fault applyFault) error {
	if plan == nil || plan.NewState == nil {
		return fmt.Errorf("apply requires a complete plan")
	}
	if outDir == "" {
		outDir = filepath.Join(projectDir, "out")
	}
	relative, err := outputRelative(projectDir, outDir)
	if err != nil {
		return err
	}
	if err := validateStatePaths(plan.NewState); err != nil {
		return err
	}
	// Check every path before the first write or deletion, including old state
	// entries that no longer occur in the generated file list.
	for _, file := range plan.GeneratedFiles {
		if err := gen.ValidatePath(file.Path); err != nil {
			return err
		}
	}
	for _, file := range plan.Files {
		if err := gen.ValidatePath(file.Path); err != nil {
			return err
		}
	}
	project, err := os.OpenRoot(projectDir)
	if err != nil {
		return fmt.Errorf("opening project root: %w", err)
	}
	defer project.Close()
	if err := pendingTransaction(project); err != nil {
		return err
	}
	if err := checkFilePath(project, relative); err != nil {
		return err
	}
	statePath := filepath.Join(".stego", "state.yaml")
	if err := checkFileTarget(project, statePath); err != nil {
		return err
	}
	for _, file := range plan.GeneratedFiles {
		if err := checkFileTarget(project, projectFilePath(relative, file.Path)); err != nil {
			return err
		}
	}
	for _, file := range plan.Files {
		if err := checkFileTarget(project, projectFilePath(relative, file.Path)); err != nil {
			return err
		}
	}
	if err := verifyPlanLocation(plan, projectDir, outDir); err != nil {
		return err
	}
	if err := verifySnapshots(project, plan.snapshots); err != nil {
		return err
	}
	lock, err := lockProject(project)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := pendingTransaction(project); err != nil {
		return err
	}
	// Another apply can finish between the first snapshot check and the lock.
	if err := verifySnapshots(project, plan.snapshots); err != nil {
		return err
	}
	return commitTransaction(project, projectDir, plan, relative, fault)
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
	// Collect the baseline component set from the archetype, default_auth,
	// and mixins. Override-component replacement is handled by ports.Resolve().
	seen := make(map[string]bool)
	var names []string

	for _, name := range archetype.Components {
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}

	if archetype.DefaultAuth != "" {
		if !seen[archetype.DefaultAuth] {
			seen[archetype.DefaultAuth] = true
			names = append(names, archetype.DefaultAuth)
		}
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
) (*Plan, error) {
	root, err := os.OpenRoot(projectDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	relative, err := outputRelative(projectDir, outDir)
	if err != nil {
		return nil, err
	}
	snapshots := make(map[string]fileSnapshot)
	serviceHash := HashBytes(serviceData)

	existingHashes := make(map[string]string)
	if existingState.LastApplied != nil && existingState.LastApplied.Files != nil {
		existingHashes = existingState.LastApplied.Files
	}

	var planned []PlannedFile
	newFileHashes := make(map[string]string)

	for _, f := range generatedFiles {
		content := f.Bytes()
		if len(content) > maxTrackedFileBytes {
			return nil, fmt.Errorf("generated file %s exceeds the %d-byte tracking limit", f.Path, maxTrackedFileBytes)
		}
		hash := HashBytes(content)
		// The application owns go.mod. Go tools can update it after apply.
		if f.Path != "go.mod" {
			newFileHashes[f.Path] = hash
		}

		name := projectFilePath(relative, f.Path)
		_, snapshot, err := readSnapshot(root, name, maxTrackedFileBytes, false)
		if err != nil {
			return nil, err
		}
		snapshots[name] = snapshot
		action := ActionUpdate
		if !snapshot.Exists {
			action = ActionGenerate
		} else if snapshot.Hash == hash {
			action = ActionUnchanged
		}
		planned = append(planned, PlannedFile{Path: f.Path, Action: action})
	}

	// Detect orphaned files: tracked in previous state but no longer generated.
	for path := range existingHashes {
		if path == "go.mod" {
			continue // Transfer ownership from older state without deletion.
		}
		if _, stillGenerated := newFileHashes[path]; !stillGenerated {
			name := projectFilePath(relative, path)
			_, snapshot, err := readSnapshot(root, name, maxTrackedFileBytes, false)
			if err != nil {
				return nil, err
			}
			snapshots[name] = snapshot
			planned = append(planned, PlannedFile{Path: path, Action: ActionDelete})
		}
	}

	sort.Slice(planned, func(i, j int) bool {
		return planned[i].Path < planned[j].Path
	})
	if err := validateFileLayout(sortedKeys(snapshots)); err != nil {
		return nil, err
	}

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

	oldStateData, err := yaml.Marshal(existingState)
	if err != nil {
		return nil, err
	}
	newStateData, err := yaml.Marshal(newState)
	if err != nil {
		return nil, err
	}
	projectPath, err := filepath.Abs(projectDir)
	if err != nil {
		return nil, err
	}
	outputPath, err := filepath.Abs(outDir)
	if err != nil {
		return nil, err
	}
	return &Plan{
		StateChanged: string(oldStateData) != string(newStateData),
		snapshots:    snapshots, projectDir: projectPath, outDir: outputPath,
		Files:          planned,
		EntityChanges:  entityChanges,
		GeneratedFiles: generatedFiles,
		NewState:       newState,
	}, nil
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
	if plan.StateChanged {
		result.WriteString("  update:   compiler state\n")
	}
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

// conventionOverrideKeys lists YAML override keys that correspond to archetype
// convention fields rather than port binding overrides. These keys must be
// excluded from port resolution and instead applied to the conventions struct.
var conventionOverrideKeys = map[string]bool{
	"cors": true,
}

// isConventionOverrideKey returns true if the given override key is a convention
// override rather than a port binding override.
func isConventionOverrideKey(key string) bool {
	return conventionOverrideKeys[key]
}

// applyConventionOverrides returns a copy of the archetype conventions with any
// service-level convention overrides applied. Currently supports:
//   - cors: "disabled" → sets CORS to "" (suppresses CORS middleware generation)
//   - cors: "enabled"  → keeps CORS as "enabled" (explicit opt-in, same as default)
func applyConventionOverrides(base types.Convention, overrides map[string]any) types.Convention {
	conv := base
	if corsVal, ok := overrides["cors"]; ok {
		if strVal, ok := corsVal.(string); ok {
			switch strVal {
			case "disabled":
				conv.CORS = ""
			case "enabled":
				conv.CORS = "enabled"
			}
		}
	}
	return conv
}

// validateUniqueFilePaths checks that no two files in the collection share the
// same output path. Duplicate paths mean one source's output would silently
// overwrite another's.
func validateUniqueFilePaths(files []gen.File) error {
	seen := make(map[string]bool, len(files))
	var duplicates []string
	var layout []string
	for _, f := range files {
		if err := gen.ValidatePath(f.Path); err != nil {
			return err
		}
		if seen[f.Path] {
			duplicates = append(duplicates, f.Path)
		}
		seen[f.Path] = true
		name := "out/" + f.Path
		if isProjectRootFile(f.Path) {
			name = f.Path
		}
		layout = append(layout, name)
	}
	if len(duplicates) > 0 {
		sort.Strings(duplicates)
		return fmt.Errorf("duplicate generated file paths: %s", strings.Join(duplicates, ", "))
	}
	return validateFileLayout(layout)
}
