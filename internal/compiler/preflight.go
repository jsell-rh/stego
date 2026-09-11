package compiler

import (
	"fmt"
	"path/filepath"
	"sort"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/types"
)

type resolvedCompilation struct {
	Components                       map[string]*types.Component
	Names                            []string
	Contexts                         map[string]gen.Context
	InputSnapshots                   map[string]fileSnapshot
	OutDir, OutDirName, SlotsPackage string
}

// prepareComponents resolves each context once. All checks finish before rendering.
func prepareComponents(input ReconcilerInput, source *compilationSource, baselineNames []string, components map[string]*types.Component) (*resolvedCompilation, error) {
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

	resolved := &resolvedCompilation{Components: components, Names: componentNames, Contexts: map[string]gen.Context{}, InputSnapshots: map[string]fileSnapshot{}, OutDir: outDir, OutDirName: outDirName, SlotsPackage: slotsPackage}
	for _, compName := range componentNames {
		comp := components[compName]
		generator := input.Generators[compName]
		ctx := gen.Context{
			Conventions:     conventions,
			Entities:        svcDecl.Entities,
			Collections:     svcDecl.Collections,
			SlotBindings:    svcDecl.Slots,
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
			PeerNamespaces:  peerNamespaces,
			PeerConfigs:     peerConfigs,
			StorageContract: generatedImportPath(input.ModuleName, outDirName, gen.StorageContractNamespace),
			EventsContract:  generatedImportPath(input.ModuleName, outDirName, gen.EventsContractNamespace),
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
	for _, name := range componentNames {
		if validator, ok := input.Generators[name].(gen.ContextValidator); ok {
			if err := validator.ValidateContext(resolved.Contexts[name]); err != nil {
				return nil, fmt.Errorf("component %q preflight: %w", name, err)
			}
		}
	}
	return resolved, nil
}
