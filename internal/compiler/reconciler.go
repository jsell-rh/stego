package compiler

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/jsell-rh/stego/internal/buildidentity"
	"github.com/jsell-rh/stego/internal/gen"
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

	// HandEdited lists, in path order, each generated file that the record
	// lists and that is missing or no longer matches its recorded hash
	// (ST-GEN-SP01). Apply refuses such a plan unless ReplaceHandEdits is set.
	HandEdited []string

	// ReplaceHandEdits states that the author lets this apply replace the
	// hand-edited files (stego apply --replace-hand-edits).
	ReplaceHandEdits bool

	// saved is the saved plan that apply carries out, when it is given one.
	saved *SavedPlan

	inputManifestSHA string
	registry         *registry.Registry
	requirements     map[string]string
	// runtime is the archetype's runtime pin when generated code imports the
	// runtime library, and nil otherwise.
	runtime    *registry.RuntimePin
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
	return p.hasOutputChanges()
}

func (p *Plan) hasOutputChanges() bool {
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
	// RegistryDirs selects all composed sources. If empty, RegistryDir is used.
	RegistryDirs []string

	// Generators maps component name to its Generator implementation.
	Generators map[string]gen.Generator

	// GoVersion is the Go version for go.mod (e.g. "1.22").
	GoVersion string

	// ModuleName is the Go module path (e.g. "github.com/myorg/user-service").
	ModuleName string

	// RegistrySHA is the supplied registry reference, which can be a local
	// label. Captured registry content has a separate SHA-256 identity.
	RegistrySHA string

	// OutDir is the output directory for generated files. Defaults to
	// filepath.Join(ProjectDir, "out") if empty.
	OutDir string

	// ReadFill reads the collection that each fill named in a slot binding
	// serves. Validation needs it when a slot binding names a fill whose
	// fill.yaml exists; cmd/stego gives fill.ReadCollection.
	ReadFill FillReader
	// ReadFillSlot reads the slot that each fill named by a command, a query or a worker implements;
	// cmd/stego gives fill.ReadImplements.
	ReadFillSlot FillSlotReader

	// QualifyFills checks each fill that a slot binding names: that it fits
	// its binding and that it is qualified. Validate, plan and apply stop on
	// each fault it gives. cmd/stego gives fill.QualificationFaults.
	QualifyFills FillQualifier

	// CheckOnly marks a plan that stego validate makes only to read the pins it uses. Such a plan is never
	// written, so Reconcile does not refuse it for a part of the declaration that no component can generate yet.
	CheckOnly bool
}

// FillSlot is one slot of the active component set, or of a mixin of the
// declaration, with the path of its contract in the registry.
type FillSlot struct {
	Ref      types.SlotRef
	Contract string
}

// FillBinding is one fill that a slot binding names, with the slot and the
// collection of the binding, and where the declaration names the fill, such
// as slots[1].gate[0].
type FillBinding struct {
	Fill       string
	Slot       string
	Collection string
	Path       string
}

// FillFault is one fault of a bound fill at one binding: the rule it breaks,
// the binding's path in the declaration, the fill it names and the fault in a
// sentence.
type FillFault struct {
	Rule    string
	Path    string
	Found   string
	Message string
}

// FillContracts reads a slot contract and the files it imports from the
// active registry.
type FillContracts interface {
	ReadFile(name string) ([]byte, error)
	ReadProtoImport(name string) ([]byte, error)
}

// FillCheck is what a check of the fills needs from the declaration and the
// active registry: each active slot with its contract, each fill that a slot
// binding names, the declared collections, and the registry's contracts.
type FillCheck struct {
	ProjectDir  string
	Slots       []FillSlot
	Bindings    []FillBinding
	Collections []string
	Contracts   FillContracts
	// Module is the Go module path of the service, whose package out/slots
	// holds the slot contracts.
	Module string
	// FillModules are the modules that the declaration lets fills import.
	FillModules []string
	// CommandFills are the fills that the commands of the declaration name, with the import path of the command
	// slot contract, whose import closure the qualifier checks (ST-SUP-RC32-OC03).
	CommandFills []CommandFill
}

// CommandFill is the one fill of one command: the fill, the command, where the declaration names it, such as
// commands[0].fill, and the import path of the command slot contract package that the fill may import.
type CommandFill struct {
	Fill     string
	Command  string
	Path     string
	Contract string
}

// FillQualifier gives a fault for each binding of a fill that does not fit
// that binding or is not qualified. The compiler owns this port and Extending's
// fill package implements it (ST-STD-CODE-R03).
type FillQualifier func(check FillCheck) ([]FillFault, error)

// FillReader reads the fill manifest (fill.yaml) at path and gives the
// collection the fill serves. The compiler owns this port and Extending's
// fill package implements it, because Generating and Declaring may not import
// Extending (ST-STD-CODE-R03).
type FillReader func(path string) (collection string, err error)

// FillSlotReader reads the fill manifest (fill.yaml) at path and gives the slot the fill implements, as
// component.slot. The compiler owns this port and Extending's fill package implements it (ST-STD-CODE-R03).
type FillSlotReader func(path string) (slot string, err error)

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
	if !input.CheckOnly {
		if err := authorizationGeneratorMissing(source.Service, source.Registry, input.Generators); err != nil {
			return nil, err
		}
		if err := selectedComponentMissing(source.Service, source.Registry); err != nil {
			return nil, err
		}
	}
	serviceData, svcDecl, reg := source.ServiceData, source.Service, source.Registry
	existingState, inputSnapshots, moduleData, err := captureProjectInputs(input.ProjectDir, serviceData)
	if err != nil {
		return nil, err
	}
	resolved := source.Resolved
	components, componentNames := resolved.Components, resolved.Names
	outDir, outDirName, slotsPackage := resolved.OutDir, resolved.OutDirName, resolved.SlotsPackage
	for name, snapshot := range resolved.InputSnapshots {
		if before, exists := inputSnapshots[name]; exists && before != snapshot {
			return nil, fmt.Errorf("input %q changed during compilation", name)
		}
		inputSnapshots[name] = snapshot
	}

	// Every generated file may import the runtime library only at the
	// archetype's pin (ST-SUP-INV10); the registry refuses an archetype
	// without one.
	archetype := reg.Archetype(svcDecl.Archetype)
	if archetype == nil || archetype.Runtime == nil {
		return nil, fmt.Errorf("archetype %q pins no runtime library", svcDecl.Archetype)
	}
	pin := *archetype.Runtime
	if err := checkPinMove(existingState, archetype); err != nil {
		return nil, err
	}
	if err := checkDataPathSnapshot(existingState, componentNames, resolved.RuntimeData); err != nil {
		return nil, err
	}
	importsRuntime := false

	// Run all component generators in archetype-declared order.
	var allFiles []gen.File
	var wirings []ComponentWiring

	for _, compName := range componentNames {
		comp := components[compName]
		generator := input.Generators[compName]

		ctx := resolved.Contexts[compName]
		if resolved.RuntimeData && slices.Contains(authProviders([]string{compName}, components), compName) {
			// The verifier of the runtime library takes the place of the auth provider on the runtime data path,
			// with its declared settings (ST-SUP-RP13); its own package would be dead code.
			continue
		}

		files, wiring, err := generator.Generate(ctx)
		if err != nil {
			return nil, fmt.Errorf("generator %q: %w", compName, err)
		}
		if err := requireUnchangedGoModels(compName, generator, ctx, resolved); err != nil {
			return nil, err
		}
		if declared, ok := resolved.HTTPRoutes[compName]; ok {
			actual, err := gen.WiringHTTPRoutes(wiring)
			if err != nil {
				return nil, fmt.Errorf("generator %q HTTP routes: %w", compName, err)
			}
			if !slices.Equal(declared, actual) {
				return nil, fmt.Errorf("generator %q HTTP routes differ from its preflight declaration", compName)
			}
		}

		if err := validateGeneratorNamespace(compName, comp, files); err != nil {
			return nil, err
		}
		uses, err := runtimeImports("component "+compName, files, pin)
		if err != nil {
			return nil, err
		}
		importsRuntime = importsRuntime || uses
		var wired []string
		if wiring != nil {
			wired = wiring.Imports
		}
		for _, path := range wired {
			uses, err := runtimeImport(path, pin)
			if err != nil {
				return nil, fmt.Errorf("component %s wires the import %s into main.go: %w", compName, path, err)
			}
			importsRuntime = importsRuntime || uses
		}

		allFiles = append(allFiles, files...)
		wirings = append(wirings, ComponentWiring{Name: compName, Wiring: wiring})
	}

	// On the runtime data path the compiler wires the runtime library into main.go (ST-SUP-RC01-OC41).
	if resolved.RuntimeData {
		files, wiring, err := runtimeWiring(runtimeWiringInput{ModuleName: input.ModuleName, OutDirName: outDirName, Service: svcDecl,
			ServiceData: serviceData, Components: components, Names: componentNames, Wirings: wirings})
		if err != nil {
			return nil, err
		}
		uses, err := runtimeImports("the runtime wiring", files, pin)
		if err != nil {
			return nil, err
		}
		importsRuntime = importsRuntime || uses
		allFiles = append(allFiles, files...)
		wirings = append(wirings, wiring)
	}

	// Generate slot interfaces and operators if slots are configured.
	slotFiles, err := generateSlotFiles(slotsPackage, components, svcDecl, reg)
	if err != nil {
		return nil, fmt.Errorf("generating slot files: %w", err)
	}
	allFiles = append(allFiles, slotFiles...)
	uses, err := runtimeImports("the slot generator", slotFiles, pin)
	if err != nil {
		return nil, err
	}
	importsRuntime = importsRuntime || uses
	var runtime *registry.RuntimePin
	if importsRuntime {
		runtime = &pin
	}

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
		Runtime:      runtime,
	}
	sharedFiles, err := Assemble(assemblerInput)
	if err != nil {
		return nil, fmt.Errorf("assembling shared files: %w", err)
	}
	for i, file := range sharedFiles {
		if file.Path == "go.mod" {
			sharedFiles[i], err = mergeCapturedModule(moduleData, inputSnapshots["go.mod"].Exists, file)
			if err != nil {
				return nil, err
			}
		}
	}
	if runtime != nil {
		sum, err := mergeProjectSums(input.ProjectDir, inputSnapshots["go.sum"], *runtime)
		if err != nil {
			return nil, err
		}
		sharedFiles = append(sharedFiles, sum)
	}
	allFiles = append(allFiles, sharedFiles...)

	// Validate no duplicate file paths across all sources (generators, slots,
	// assembler). A duplicate means one generator's output would silently
	// overwrite another's.
	if err := validateUniqueFilePaths(allFiles); err != nil {
		return nil, err
	}

	manifest, err := newInputManifest(input, outDirName, resolved.HoldsFills, inputSnapshots)
	if err != nil {
		return nil, err
	}
	// Compute plan by comparing generated files against existing state.
	pins, err := componentPins(components, reg)
	if err != nil {
		return nil, err
	}
	plan, err := computePlan(allFiles, existingState, serviceData, svcDecl.Name, svcDecl.Entities, pins, outDir, input.ProjectDir, input.RegistrySHA, reg.ContentHash(), manifest)
	if err != nil {
		return nil, err
	}
	if err := bindPlanInputs(plan, input.ProjectDir, inputSnapshots); err != nil {
		return nil, err
	}
	plan.inputManifestSHA = manifest.SHA256
	plan.registry = reg
	if err := plan.verifyRegistry(); err != nil {
		return nil, err
	}
	plan.requirements, err = moduleRequirements(wirings, runtime)
	if err != nil {
		return nil, err
	}
	plan.runtime = runtime
	plan.NewState.LastApplied.Archetype = &ArchetypeState{Name: archetype.Name, Version: archetype.Version, Runtime: &pin, ImportsRuntime: importsRuntime}
	plan.NewState.LastApplied.DataPath = dataPathName(resolved.RuntimeData)
	if plan.StateChanged, err = stateDiffers(existingState, plan.NewState); err != nil {
		return nil, err
	}
	return plan, nil
}

// Apply writes all generated files to disk under outDir (or projectDir for
// project-root files like go.mod), removes orphaned files, and saves the
// new state.
func Apply(plan *Plan, projectDir, outDir string) error {
	return applyWithFault(plan, projectDir, outDir, stopHook)
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
	if err := refuseHandEdits(plan); err != nil {
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
	if err := plan.verifyRegistry(); err != nil {
		return err
	}
	if plan.saved != nil {
		if err := plan.saved.StillApplies(plan); err != nil {
			return err
		}
	}
	if err := commitTransaction(project, projectDir, plan, relative, fault); err != nil {
		return err
	}
	return removeSavedPlan(project)
}

// isProjectRootFile returns true for files that should be placed at the project
// root. The application owns its module and checksum files.
func isProjectRootFile(filePath string) bool {
	return filePath == "go.mod" || filePath == "go.sum"
}

// collectComponentNames assembles the ordered list of component names from the
// archetype, default_auth, and any mixin-added components.
func collectComponentNames(archetype *registry.Archetype, svcDecl *types.ServiceDeclaration, reg *registry.Registry) ([]string, error) {
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

	// The infrastructure components that the declared settings select, which the registry holds
	// (ST-GEN-INV37). A selected component is not part of the archetype or a mixin.
	for _, s := range selectComponents(svcDecl) {
		if !seen[s.Component] && reg.Component(s.Component) != nil {
			seen[s.Component] = true
			names = append(names, s.Component)
		}
	}

	return names, nil
}

// resolveComponentConfig merges a component's default config values with any
// service-level overrides for that component.
func resolveComponentConfig(comp *registry.Component, svcDecl *types.ServiceDeclaration) map[string]any {
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
func generateSlotFiles(slotsPackage string, components map[string]*registry.Component, svcDecl *types.ServiceDeclaration, reg *registry.Registry) ([]gen.File, error) {
	if slotsPackage == "" {
		return nil, nil
	}

	pkgName := filepath.Base(slotsPackage)

	// The package holds the contract of every slot of the active component
	// set and of the declaration's mixins, bound or not, so that a fill of any
	// of them compiles before it is bound (ST-EXT-INV05).
	slotNames := make(map[string]bool)
	for _, sb := range svcDecl.Slots {
		slotNames[sb.Slot] = true
	}
	for _, comp := range components {
		for _, sd := range comp.Slots {
			slotNames[sd.Name] = true
		}
	}
	for _, mixinName := range svcDecl.Mixins {
		if mixin := reg.Mixin(mixinName); mixin != nil {
			for _, sd := range mixin.AddsSlots {
				slotNames[sd.Name] = true
			}
		}
	}

	// Find the slot definitions and their owning components or mixins.
	type slotInfo struct {
		definition registry.SlotDefinition
		// protoDir is the directory containing the slots/ subdirectory for this
		// slot's proto file, relative to the captured registry root.
		protoDir string
	}
	slotDefs := make(map[string]slotInfo)
	for _, comp := range components {
		for _, sd := range comp.Slots {
			if slotNames[sd.Name] {
				slotDefs[sd.Name] = slotInfo{
					definition: sd,
					protoDir:   filepath.Join("components", comp.Name),
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
						protoDir:   filepath.Join("mixins", mixin.Name),
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

		protoData, err := reg.ReadFile(filepath.ToSlash(protoPath))
		if err != nil {
			return nil, fmt.Errorf("opening proto for slot %q: %w", slotName, err)
		}
		parsed, err := slot.ParseProto(bytes.NewReader(protoData))
		if err != nil {
			return nil, fmt.Errorf("parsing proto for slot %q: %w", slotName, err)
		}

		var imports []*slot.ProtoFile
		for _, name := range parsed.Imports {
			data, err := reg.ReadProtoImport(name)
			if err != nil {
				return nil, fmt.Errorf("slot %q import %q: %w", slotName, name, err)
			}
			parsedImport, err := slot.ParseProto(bytes.NewReader(data))
			if err != nil {
				return nil, fmt.Errorf("slot %q import %q: %w", slotName, name, err)
			}
			imports = append(imports, parsedImport)
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

// refuseHandEdits holds ST-GEN-UC03-R03: apply names every hand-edited
// generated file and writes nothing, unless the author lets it replace them.
func refuseHandEdits(plan *Plan) error {
	if len(plan.HandEdited) == 0 || plan.ReplaceHandEdits {
		return nil
	}
	return fmt.Errorf("apply would replace %d hand-edited generated file(s), and writes nothing: %s; restore them, or run stego apply --replace-hand-edits to replace them", len(plan.HandEdited), strings.Join(plan.HandEdited, ", "))
}

// componentPins gives each component of the active set with its version and
// its component digest, from the registry inputs that the run captured.
func componentPins(components map[string]*registry.Component, reg *registry.Registry) (map[string]ComponentState, error) {
	files := reg.InputFiles()
	pins := make(map[string]ComponentState, len(components))
	for name, component := range components {
		digest, err := ComponentDigest(files, name)
		if err != nil {
			return nil, err
		}
		pins[name] = ComponentState{Version: component.Version, SHA: digest}
	}
	return pins, nil
}

// validateGeneratorNamespace holds ST-GEN-INV07: a component that generates
// files declares an output namespace, and every file is inside it.
func validateGeneratorNamespace(name string, component *registry.Component, files []gen.File) error {
	if len(files) == 0 {
		return nil
	}
	if component.OutputNamespace == "" {
		return fmt.Errorf("generator %q produced %d file(s) but component declares no output_namespace — "+
			"all components that generate files must declare an output_namespace", name, len(files))
	}
	if err := gen.ValidateNamespace(component.OutputNamespace, files); err != nil {
		return fmt.Errorf("generator %q: %w", name, err)
	}
	return nil
}

// computePlan compares generated files against existing state to determine
// which files need to be written and what entity changes occurred.
func computePlan(
	generatedFiles []gen.File,
	existingState *State,
	serviceData []byte,
	serviceName string,
	entities []types.Entity,
	pins map[string]ComponentState,
	outDir string,
	projectDir string,
	registrySHA string,
	registryContentHash string,
	manifest *InputManifest,
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
		if !isProjectRootFile(f.Path) {
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
		if isProjectRootFile(f.Path) && action != ActionUnchanged && manifest != nil {
			// The run writes the module file that the generated code builds
			// with, so the record names it as the run leaves it, and a second
			// apply from the same inputs changes nothing (ST-GEN-UC04-R01).
			mode := defaultOutputMode()
			if snapshot.Exists {
				mode = snapshot.Mode.Perm()
			}
			manifest.Files[name] = InputFile{Exists: true, SHA256: hash, Mode: uint32(mode)}
			manifest.SHA256 = manifest.digest()
		}
	}

	// Detect orphaned files: tracked in previous state but no longer generated.
	for path := range existingHashes {
		if isProjectRootFile(path) {
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

	var handEdited []string
	for path, recorded := range existingHashes {
		if isProjectRootFile(path) {
			continue
		}
		name := projectFilePath(relative, path)
		snapshot, read := snapshots[name]
		if !read {
			var err error
			if _, snapshot, err = readSnapshot(root, name, maxTrackedFileBytes, false); err != nil {
				return nil, err
			}
		}
		if !snapshot.Exists || snapshot.Hash != recorded {
			handEdited = append(handEdited, path)
		}
	}
	sort.Strings(handEdited)

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

	compilerBuild := buildidentity.Current()
	newState := &State{
		FormatVersion: StateFormatVersion,
		LastApplied: &AppliedState{
			Inputs:                manifest,
			CompilerBuild:         &compilerBuild,
			ServiceName:           serviceName,
			ServiceHash:           serviceHash,
			RegistrySHA:           registrySHA,
			RegistryContentSHA256: registryContentHash,
			Components:            pins,
			Entities:              entitySnapshot,
			Files:                 newFileHashes,
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
		HandEdited:   handEdited,
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
