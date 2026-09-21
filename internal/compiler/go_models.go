package compiler

import (
	"fmt"
	"reflect"

	"github.com/jsell-rh/stego/internal/gen"
)

func resolveGoModelSources(input ReconcilerInput, resolved *resolvedCompilation) error {
	sources := map[string]gen.GoModelSource{}
	for _, name := range resolved.Names {
		provider, ok := input.Generators[name].(gen.GoModelProvider)
		if !ok {
			continue
		}
		ctx := resolved.Contexts[name]
		if err := gen.ValidateGoPackageNamespace(ctx.OutputNamespace); err != nil {
			return err
		}
		models, err := provider.GoModels(ctx)
		if err != nil {
			return fmt.Errorf("component %q Go models: %w", name, err)
		}
		if err := gen.ValidateGoModels(models); err != nil {
			return fmt.Errorf("component %q Go models: %w", name, err)
		}
		sources[name] = gen.GoModelSource{
			ImportPath: generatedImportPath(ctx.ModuleName, ctx.OutDirName, ctx.OutputNamespace),
			Models:     gen.CloneGoModels(models),
		}
	}
	resolved.GoModelSources = gen.CloneGoModelSources(sources)
	for _, name := range resolved.Names {
		ctx := resolved.Contexts[name]
		ctx.GoModelSources = gen.CloneGoModelSources(sources)
		resolved.Contexts[name] = ctx
	}
	return nil
}

func requireUnchangedGoModels(name string, generator gen.Generator, ctx gen.Context, resolved *resolvedCompilation) error {
	saved, exists := resolved.GoModelSources[name]
	if !exists {
		return nil
	}
	provider := generator.(gen.GoModelProvider)
	actual, err := provider.GoModels(ctx)
	if err != nil {
		return fmt.Errorf("component %q Go models: %w", name, err)
	}
	if !reflect.DeepEqual(saved.Models, actual) {
		return fmt.Errorf("component %q Go models differ from its preflight declaration", name)
	}
	return nil
}
