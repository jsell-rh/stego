package compiler

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sort"

	"github.com/jsell-rh/stego/internal/declaration"
	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/registry"
	"github.com/jsell-rh/stego/internal/types"
)

type resolvedCompilation struct {
	Components                       map[string]*registry.Component
	Names                            []string
	Contexts                         map[string]gen.Context
	HTTPRoutes                       map[string][]gen.HTTPRoute
	GoModelSources                   map[string]gen.GoModelSource
	InputSnapshots                   map[string]fileSnapshot
	OutDir, OutDirName, SlotsPackage string
	HoldsFills                       bool
}

// prepareComponents resolves each context once. All checks finish before rendering.
func prepareComponents(input ReconcilerInput, source *compilationSource, baselineNames []string, components map[string]*registry.Component) (*resolvedCompilation, error) {
	svcDecl := source.Service
	archetype := source.Registry.Archetype(svcDecl.Archetype)
	conventions := applyConventionOverrides(archetype.Conventions, svcDecl.Overrides)
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
	// A project that holds fills gets the package too, before any fill is
	// bound, so that its fills compile and their tests run (ST-EXT-INV05).
	slotsPackage := ""
	fills := holdsFills(input.ProjectDir)
	if len(svcDecl.Slots) > 0 || fills {
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
	if err := gen.ValidateGoImportNamespace(outDirName); err != nil {
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
	peerConfigs := make(map[string]map[string]any, len(componentNames))
	for _, compName := range componentNames {
		comp := components[compName]
		peerConfigs[compName] = resolveComponentConfig(comp, svcDecl)
		if comp.OutputNamespace != "" {
			peerNamespaces[compName] = comp.OutputNamespace
		}
	}

	// The ports that the declaration binds: each override whose value is a
	// component name (ST-DECL-EN10 binds the audit sink this way).
	portBindings := map[string]string{}
	for key, value := range svcDecl.Overrides {
		if component, ok := value.(string); ok && !declaration.IsConventionOverrideKey(key) {
			portBindings[key] = component
		}
	}

	tenancy := pooledTenancy(svcDecl, componentNames)
	facts, err := compositionFacts(input, svcDecl, components)
	if err != nil {
		return nil, err
	}
	resolved := &resolvedCompilation{Components: components, Names: componentNames, Contexts: map[string]gen.Context{}, InputSnapshots: map[string]fileSnapshot{}, OutDir: outDir, OutDirName: outDirName, SlotsPackage: slotsPackage, HoldsFills: fills}
	for _, compName := range componentNames {
		comp := components[compName]
		generator := input.Generators[compName]
		ctx := gen.Context{
			Conventions:     conventions,
			Entities:        svcDecl.Entities,
			Collections:     svcDecl.Collections,
			SlotBindings:    svcDecl.Slots,
			Observations:    svcDecl.Observations,
			Emitters:        declaredEmitters(svcDecl),
			ModuleName:      input.ModuleName,
			GoVersion:       input.GoVersion,
			SlotsPackage:    slotsPackage,
			ComponentConfig: peerConfigs[compName],
			OutputNamespace: comp.OutputNamespace,
			OutDirName:      outDirName,
			AuthPackage:     authPackage,
			BasePath:        svcDecl.BasePath,
			ServiceName:     svcDecl.Name,
			ErrorTypeBase:   svcDecl.ErrorTypeBase,
			Audit:           svcDecl.Audit,
			PortBindings:    portBindings,
			Idempotency:     svcDecl.Idempotency,
			PeerNamespaces:  peerNamespaces,
			PeerConfigs:     peerConfigs,
			StorageContract: generatedImportPath(input.ModuleName, outDirName, gen.StorageContractNamespace),
			EventsContract:  generatedImportPath(input.ModuleName, outDirName, gen.EventsContractNamespace),
			Tenancy:         tenancy,
			Queries:         svcDecl.Queries,
			FillSlots:       maps.Clone(facts.FillSlots),
		}
		if provider, ok := generator.(gen.InputProvider); ok {
			names, err := provider.InputFiles(ctx.ComponentConfig)
			if err != nil {
				return nil, fmt.Errorf("generator %q inputs: %w", compName, err)
			}
			ctx.Inputs, err = captureGeneratorInputs(input.ProjectDir, outDirName, names, resolved.InputSnapshots)
			if err != nil {
				return nil, fmt.Errorf("generator %q inputs: %w", compName, err)
			}
		}

		resolved.Contexts[compName] = ctx
	}
	if err := resolveGoModelSources(input, resolved); err != nil {
		return nil, err
	}
	for _, name := range componentNames {
		if validator, ok := input.Generators[name].(gen.ContextValidator); ok {
			if err := validator.ValidateContext(resolved.Contexts[name]); err != nil {
				return nil, fmt.Errorf("component %q preflight: %w", name, err)
			}
		}
	}
	resolved.HTTPRoutes = map[string][]gen.HTTPRoute{}
	for _, name := range componentNames {
		if provider, ok := input.Generators[name].(gen.HTTPRouteProvider); ok {
			routes, err := provider.HTTPRoutes(resolved.Contexts[name])
			if err != nil {
				return nil, fmt.Errorf("component %q HTTP routes: %w", name, err)
			}
			resolved.HTTPRoutes[name] = append([]gen.HTTPRoute(nil), routes...)
		}
	}
	if err := gen.ValidateHTTPRouteGroups(resolved.HTTPRoutes); err != nil {
		return nil, err
	}
	return resolved, nil
}

// holdsFills tells if the fills folder of the project holds at least one
// fill folder. An empty fills folder, as stego init makes it, does not count:
// Git keeps no empty folder, so a clone of the project would generate other
// files.
func holdsFills(projectDir string) bool {
	entries, err := os.ReadDir(filepath.Join(projectDir, "fills"))
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return true
		}
	}
	return false
}

// declaredEmitters lists the commands, queries, workers and slot bindings of the declaration, in this order and in
// declaration order, with the names that an observation's Emitted by uses: a slot binding as collection.slot
// (ST-DECL-INV54, ST-SUP-RC34-OC03).
func declaredEmitters(svcDecl *types.ServiceDeclaration) []gen.Emitter {
	var emitters []gen.Emitter
	for _, command := range svcDecl.Commands {
		emitters = append(emitters, gen.Emitter{Name: command.Name, Kind: gen.EmitterCommand})
	}
	for _, query := range svcDecl.Queries {
		emitters = append(emitters, gen.Emitter{Name: query.Name, Kind: gen.EmitterQuery})
	}
	for _, worker := range svcDecl.Workers {
		emitters = append(emitters, gen.Emitter{Name: worker.Name, Kind: gen.EmitterWorker})
	}
	for _, binding := range svcDecl.Slots {
		if binding.Collection == "" {
			continue
		}
		emitters = append(emitters, gen.Emitter{Name: binding.Collection + "." + binding.Slot, Kind: gen.EmitterSlotBinding})
	}
	return emitters
}

// pooledTenancy gives the tenancy that the generators read: the key of a pooled tenancy, tenant_id when the
// declaration omits it, for a service that has the tenancy component, and nil for any other service
// (ST-SUP-RC01-OC40, ST-DECL-INV18).
func pooledTenancy(service *types.ServiceDeclaration, componentNames []string) *gen.Tenancy {
	present := false
	for _, name := range componentNames {
		if name == gen.TenancyComponent {
			present = true
		}
	}
	if !present {
		return nil
	}
	key := "tenant_id"
	if tenancy := service.Tenancy; tenancy != nil {
		if tenancy.Kind != "" && tenancy.Kind != "pooled" {
			return nil
		}
		if tenancy.Key != "" {
			key = tenancy.Key
		}
	}
	return &gen.Tenancy{Key: key}
}
