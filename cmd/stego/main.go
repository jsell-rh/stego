package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/jsell-rh/stego/internal/compiler"
	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/registry"
	"github.com/jsell-rh/stego/internal/generator/healthcheck"
	"github.com/jsell-rh/stego/internal/generator/jwtauth"
	"github.com/jsell-rh/stego/internal/generator/oteltracing"
	"github.com/jsell-rh/stego/internal/generator/postgresadapter"
	"github.com/jsell-rh/stego/internal/generator/restapi"
)

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "version":
		fmt.Println("stego", version)
	case "plan":
		if err := runPlan(); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	case "apply":
		if err := runApply(); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	case "validate":
		if err := runValidate(); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	case "drift":
		if err := runDrift(); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, "Usage: stego <command>")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Commands:")
	fmt.Fprintln(os.Stderr, "  plan      Show changes that would be applied")
	fmt.Fprintln(os.Stderr, "  apply     Generate/update code")
	fmt.Fprintln(os.Stderr, "  validate  Check service.yaml against registry")
	fmt.Fprintln(os.Stderr, "  drift     Detect hand-edits to generated files")
	fmt.Fprintln(os.Stderr, "  version   Print version")
}

func buildReconcilerInput() (compiler.ReconcilerInput, error) {
	projectDir, err := os.Getwd()
	if err != nil {
		return compiler.ReconcilerInput{}, fmt.Errorf("getting working directory: %w", err)
	}

	// Look for registry directory. Check local ./registry first, then
	// environment variable STEGO_REGISTRY.
	registryDir := filepath.Join(projectDir, "registry")
	if envReg := os.Getenv("STEGO_REGISTRY"); envReg != "" {
		registryDir = envReg
	}
	if _, err := os.Stat(registryDir); err != nil {
		return compiler.ReconcilerInput{}, fmt.Errorf("registry directory not found at %s: %w", registryDir, err)
	}

	// Module name defaults to service name; can be overridden by STEGO_MODULE.
	moduleName := os.Getenv("STEGO_MODULE")
	if moduleName == "" {
		moduleName = "github.com/example/service"
	}

	goVersion := os.Getenv("STEGO_GO_VERSION")
	if goVersion == "" {
		goVersion = "1.22"
	}

	// Try to load registry SHA from .stego/config.yaml for auditability.
	var registrySHA string
	configPath := filepath.Join(projectDir, ".stego", "config.yaml")
	if cfg, err := registry.LoadConfig(configPath); err == nil && len(cfg.Registry) > 0 {
		registrySHA = cfg.Registry[0].Ref
	}

	outDir := filepath.Join(projectDir, "out")

	return compiler.ReconcilerInput{
		ProjectDir:  projectDir,
		RegistryDir: registryDir,
		Generators:  defaultGenerators(),
		GoVersion:   goVersion,
		ModuleName:  moduleName,
		RegistrySHA: registrySHA,
		OutDir:      outDir,
	}, nil
}

func defaultGenerators() map[string]gen.Generator {
	return map[string]gen.Generator{
		"rest-api":         &restapi.Generator{},
		"postgres-adapter": &postgresadapter.Generator{},
		"jwt-auth":         &jwtauth.Generator{},
		"otel-tracing":     &oteltracing.Generator{},
		"health-check":     &healthcheck.Generator{},
	}
}

func runPlan() error {
	input, err := buildReconcilerInput()
	if err != nil {
		return err
	}

	plan, err := compiler.Reconcile(input)
	if err != nil {
		return err
	}

	fmt.Print(compiler.FormatPlan(plan))
	return nil
}

func runValidate() error {
	input, err := buildReconcilerInput()
	if err != nil {
		return err
	}

	result, err := compiler.Validate(input)
	if err != nil {
		return err
	}

	fmt.Print(compiler.FormatValidation(result))
	if result.HasErrors() {
		return fmt.Errorf("validation failed with %d error(s)", len(result.Errors))
	}
	return nil
}

func runDrift() error {
	projectDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting working directory: %w", err)
	}
	outDir := filepath.Join(projectDir, "out")

	result, err := compiler.DetectDrift(projectDir, outDir)
	if err != nil {
		return err
	}

	fmt.Print(compiler.FormatDrift(result))
	if result.HasDrift() {
		return fmt.Errorf("drift detected in %d file(s)", len(result.Modified)+len(result.Deleted))
	}
	return nil
}

func runApply() error {
	input, err := buildReconcilerInput()
	if err != nil {
		return err
	}

	plan, err := compiler.Reconcile(input)
	if err != nil {
		return err
	}

	if !plan.HasChanges() {
		fmt.Println("No changes. Infrastructure is up-to-date.")
		return nil
	}

	if err := compiler.Apply(plan, input.ProjectDir, input.OutDir); err != nil {
		return err
	}

	fmt.Print(compiler.FormatPlan(plan))
	fmt.Println("\nApply complete!")
	return nil
}
