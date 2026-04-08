package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jsell-rh/stego/internal/compiler"
	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/healthcheck"
	"github.com/jsell-rh/stego/internal/generator/jwtauth"
	"github.com/jsell-rh/stego/internal/generator/oteltracing"
	"github.com/jsell-rh/stego/internal/generator/postgresadapter"
	"github.com/jsell-rh/stego/internal/generator/restapi"
	"github.com/jsell-rh/stego/internal/parser"
	"github.com/jsell-rh/stego/internal/registry"
	"github.com/jsell-rh/stego/internal/slot"
	"github.com/jsell-rh/stego/internal/types"
	"gopkg.in/yaml.v3"
)

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	cmd := os.Args[1]

	// Handle --help and -h at top level.
	if cmd == "--help" || cmd == "-h" {
		printUsage()
		return
	}

	var err error
	switch cmd {
	case "version":
		fmt.Println("stego", version)
	case "init":
		err = runInit(os.Args[2:])
	case "plan":
		err = runPlan(os.Args[2:])
	case "apply":
		err = runApply(os.Args[2:])
	case "validate":
		err = runValidate(os.Args[2:])
	case "drift":
		err = runDrift(os.Args[2:])
	case "test":
		err = runTest(os.Args[2:])
	case "fill":
		err = runFill(os.Args[2:])
	case "registry":
		err = runRegistry(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
		printUsage()
		os.Exit(1)
	}

	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, "Usage: stego <command> [options]")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Project lifecycle:")
	fmt.Fprintln(os.Stderr, "  init            Create project from archetype")
	fmt.Fprintln(os.Stderr, "  fill create     Scaffold a new fill with generated interface")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Reconciliation:")
	fmt.Fprintln(os.Stderr, "  plan            Diff desired vs current, show changeset")
	fmt.Fprintln(os.Stderr, "  apply           Generate/update code")
	fmt.Fprintln(os.Stderr, "  drift           Detect hand-edits to generated files")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Validation:")
	fmt.Fprintln(os.Stderr, "  validate        Check service.yaml against registry")
	fmt.Fprintln(os.Stderr, "  test            Run all fill tests")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Registry:")
	fmt.Fprintln(os.Stderr, "  registry search     Query components by provides/requires/slots")
	fmt.Fprintln(os.Stderr, "  registry inspect    Show component details")
	fmt.Fprintln(os.Stderr, "  registry fills      Find existing fills for a slot")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Other:")
	fmt.Fprintln(os.Stderr, "  version         Print version")
}

// runInit implements `stego init --archetype <name>`.
func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	archetype := fs.String("archetype", "", "archetype name (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *archetype == "" {
		fs.Usage()
		return fmt.Errorf("--archetype is required")
	}

	projectDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting working directory: %w", err)
	}

	// Load registry to validate archetype exists.
	reg, err := loadRegistry(projectDir)
	if err != nil {
		return err
	}

	arch := reg.Archetype(*archetype)
	if arch == nil {
		available := make([]string, 0, len(reg.Archetypes()))
		for name := range reg.Archetypes() {
			available = append(available, name)
		}
		return fmt.Errorf("archetype %q not found in registry (available: %s)", *archetype, strings.Join(available, ", "))
	}

	// Generate service.yaml scaffold.
	servicePath := filepath.Join(projectDir, "service.yaml")
	if _, err := os.Stat(servicePath); err == nil {
		return fmt.Errorf("service.yaml already exists in %s", projectDir)
	}

	// Derive a project name from the directory name.
	projectName := filepath.Base(projectDir)

	svc := types.ServiceDeclaration{
		Kind:      "service",
		Name:      projectName,
		Archetype: *archetype,
		Language:  arch.Language,
		Entities:  []types.Entity{},
		Expose:    []types.ExposeBlock{},
	}

	svcData, err := yaml.Marshal(svc)
	if err != nil {
		return fmt.Errorf("marshaling service.yaml: %w", err)
	}

	if err := os.WriteFile(servicePath, svcData, 0644); err != nil {
		return fmt.Errorf("writing service.yaml: %w", err)
	}

	// Create .stego directory and config.yaml.
	stegoDir := filepath.Join(projectDir, ".stego")
	if err := os.MkdirAll(stegoDir, 0755); err != nil {
		return fmt.Errorf("creating .stego directory: %w", err)
	}

	configPath := filepath.Join(stegoDir, "config.yaml")
	if _, err := os.Stat(configPath); err != nil {
		// Only create if it doesn't exist.
		cfg := types.RegistryConfig{
			Registry: []types.RegistrySource{},
		}
		cfgData, err := yaml.Marshal(cfg)
		if err != nil {
			return fmt.Errorf("marshaling config.yaml: %w", err)
		}
		if err := os.WriteFile(configPath, cfgData, 0644); err != nil {
			return fmt.Errorf("writing config.yaml: %w", err)
		}
	}

	// Create fills directory.
	fillsDir := filepath.Join(projectDir, "fills")
	if err := os.MkdirAll(fillsDir, 0755); err != nil {
		return fmt.Errorf("creating fills directory: %w", err)
	}

	fmt.Printf("Initialized stego project %q with archetype %q\n", projectName, *archetype)
	fmt.Println("Created:")
	fmt.Println("  service.yaml")
	fmt.Println("  .stego/config.yaml")
	fmt.Println("  fills/")
	return nil
}

// runFill dispatches fill subcommands.
func runFill(args []string) error {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: stego fill <subcommand>")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Subcommands:")
		fmt.Fprintln(os.Stderr, "  create    Scaffold a new fill with generated interface")
		return fmt.Errorf("fill subcommand required")
	}
	switch args[0] {
	case "create":
		return runFillCreate(args[1:])
	case "--help", "-h":
		fmt.Fprintln(os.Stderr, "Usage: stego fill <subcommand>")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Subcommands:")
		fmt.Fprintln(os.Stderr, "  create    Scaffold a new fill with generated interface")
		return nil
	default:
		return fmt.Errorf("unknown fill subcommand: %s", args[0])
	}
}

// runFillCreate implements `stego fill create <name> --slot <s>`.
// Supports both `fill create <name> --slot <s>` and `fill create --slot <s> <name>`.
func runFillCreate(args []string) error {
	fs := flag.NewFlagSet("fill create", flag.ContinueOnError)
	slotName := fs.String("slot", "", "slot to implement (required)")

	// Extract the fill name from args, which may appear before flags.
	// Collect non-flag args and flag args separately, then parse flags.
	var positional []string
	var flagArgs []string
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "-") {
			flagArgs = append(flagArgs, args[i])
			// If this flag takes a value, consume the next arg too.
			if i+1 < len(args) && !strings.Contains(args[i], "=") {
				i++
				flagArgs = append(flagArgs, args[i])
			}
		} else {
			positional = append(positional, args[i])
		}
	}
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	// Append any remaining args from flag parsing.
	positional = append(positional, fs.Args()...)

	if len(positional) < 1 {
		fs.Usage()
		return fmt.Errorf("fill name is required")
	}
	fillName := positional[0]
	if *slotName == "" {
		fs.Usage()
		return fmt.Errorf("--slot is required")
	}

	projectDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting working directory: %w", err)
	}

	// Load registry to find the slot's proto definition.
	reg, err := loadRegistry(projectDir)
	if err != nil {
		return err
	}

	// Find which component(s) own this slot. Multiple components can define
	// slots with the same name (spec: "duplication is cheaper than coupling").
	var matchingComps []*types.Component
	for _, comp := range reg.Components() {
		for _, s := range comp.Slots {
			if s.Name == *slotName {
				matchingComps = append(matchingComps, comp)
				break
			}
		}
	}

	if len(matchingComps) == 0 {
		return fmt.Errorf("slot %q not found in any component", *slotName)
	}
	if len(matchingComps) > 1 {
		names := make([]string, len(matchingComps))
		for i, c := range matchingComps {
			names[i] = c.Name
		}
		sort.Strings(names)
		return fmt.Errorf("slot %q is defined by multiple components: %s — specify --component to disambiguate", *slotName, strings.Join(names, ", "))
	}
	ownerComp := matchingComps[0]

	// Create fill directory.
	fillDir := filepath.Join(projectDir, "fills", fillName)
	if _, err := os.Stat(fillDir); err == nil {
		return fmt.Errorf("fill directory already exists: %s", fillDir)
	}
	if err := os.MkdirAll(fillDir, 0755); err != nil {
		return fmt.Errorf("creating fill directory: %w", err)
	}

	// Write fill.yaml.
	fill := types.Fill{
		Kind:       "fill",
		Name:       fillName,
		Implements: ownerComp.Name + "." + *slotName,
	}
	fillData, err := yaml.Marshal(fill)
	if err != nil {
		return fmt.Errorf("marshaling fill.yaml: %w", err)
	}
	if err := os.WriteFile(filepath.Join(fillDir, "fill.yaml"), fillData, 0644); err != nil {
		return fmt.Errorf("writing fill.yaml: %w", err)
	}

	// Try to generate interface stub from proto.
	registryDir := resolveRegistryDir(projectDir)
	protoPath := filepath.Join(registryDir, "components", ownerComp.Name, "slots", *slotName+".proto")

	if _, err := os.Stat(protoPath); err == nil {
		protoFile, err := os.Open(protoPath)
		if err != nil {
			return fmt.Errorf("opening proto file: %w", err)
		}
		defer protoFile.Close()

		proto, err := slot.ParseProto(protoFile)
		if err != nil {
			return fmt.Errorf("parsing proto: %w", err)
		}

		// Sanitize fill name to a valid Go package name (replace hyphens with underscores).
		pkgName := strings.ReplaceAll(fillName, "-", "_")

		iface, err := slot.GenerateInterface(
			filepath.Join(fillDir, "interface.go"),
			pkgName,
			proto,
			nil,
		)
		if err != nil {
			return fmt.Errorf("generating interface: %w", err)
		}

		if err := os.WriteFile(filepath.Join(fillDir, "interface.go"), iface.Bytes(), 0644); err != nil {
			return fmt.Errorf("writing interface.go: %w", err)
		}
	}

	fmt.Printf("Created fill %q implementing slot %q\n", fillName, *slotName)
	fmt.Printf("  fills/%s/fill.yaml\n", fillName)
	if _, err := os.Stat(filepath.Join(fillDir, "interface.go")); err == nil {
		fmt.Printf("  fills/%s/interface.go\n", fillName)
	}
	return nil
}

// runPlan implements `stego plan`.
func runPlan(args []string) error {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

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

// runApply implements `stego apply`.
func runApply(args []string) error {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

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

// runValidate implements `stego validate`.
func runValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

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

// runDrift implements `stego drift`.
func runDrift(args []string) error {
	fs := flag.NewFlagSet("drift", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

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

// runTest implements `stego test` by delegating to `go test ./fills/...`.
func runTest(args []string) error {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	goArgs := []string{"test", "./fills/..."}
	// Pass any remaining args through to go test.
	goArgs = append(goArgs, fs.Args()...)

	cmd := exec.Command("go", goArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go test failed: %w", err)
	}
	return nil
}

// runRegistry dispatches registry subcommands.
func runRegistry(args []string) error {
	if len(args) < 1 {
		printRegistryUsage()
		return fmt.Errorf("registry subcommand required")
	}
	switch args[0] {
	case "search":
		return runRegistrySearch(args[1:])
	case "inspect":
		return runRegistryInspect(args[1:])
	case "fills":
		return runRegistryFills(args[1:])
	case "--help", "-h":
		printRegistryUsage()
		return nil
	default:
		return fmt.Errorf("unknown registry subcommand: %s", args[0])
	}
}

func printRegistryUsage() {
	fmt.Fprintln(os.Stderr, "Usage: stego registry <subcommand> [options]")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Subcommands:")
	fmt.Fprintln(os.Stderr, "  search     Query components by provides/requires/slots")
	fmt.Fprintln(os.Stderr, "  inspect    Show component details")
	fmt.Fprintln(os.Stderr, "  fills      Find existing fills for a slot")
}

// runRegistrySearch implements `stego registry search`.
func runRegistrySearch(args []string) error {
	fs := flag.NewFlagSet("registry search", flag.ContinueOnError)
	provides := fs.String("provides", "", "filter by provided port")
	requires := fs.String("requires", "", "filter by required port")
	slotFilter := fs.String("slot", "", "filter by slot name")
	if err := fs.Parse(args); err != nil {
		return err
	}

	projectDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting working directory: %w", err)
	}

	reg, err := loadRegistry(projectDir)
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "NAME\tVERSION\tPROVIDES\tREQUIRES\tSLOTS\n")

	for name, comp := range reg.Components() {
		if *provides != "" && !portListContains(comp.Provides, *provides) {
			continue
		}
		if *requires != "" && !portListContains(comp.Requires, *requires) {
			continue
		}
		if *slotFilter != "" && !slotListContains(comp.Slots, *slotFilter) {
			continue
		}

		provideNames := portNames(comp.Provides)
		requireNames := portNames(comp.Requires)
		slotNames := slotDefNames(comp.Slots)

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			name, comp.Version,
			strings.Join(provideNames, ","),
			strings.Join(requireNames, ","),
			strings.Join(slotNames, ","),
		)
	}
	w.Flush()
	return nil
}

// runRegistryInspect implements `stego registry inspect <component>`.
func runRegistryInspect(args []string) error {
	fs := flag.NewFlagSet("registry inspect", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("component name is required")
	}
	compName := fs.Arg(0)

	projectDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting working directory: %w", err)
	}

	reg, err := loadRegistry(projectDir)
	if err != nil {
		return err
	}

	comp := reg.Component(compName)
	if comp == nil {
		return fmt.Errorf("component %q not found", compName)
	}

	fmt.Printf("Name:      %s\n", comp.Name)
	fmt.Printf("Version:   %s\n", comp.Version)
	fmt.Printf("Namespace: %s\n", comp.OutputNamespace)

	if len(comp.Provides) > 0 {
		fmt.Printf("Provides:  %s\n", strings.Join(portNames(comp.Provides), ", "))
	}
	if len(comp.Requires) > 0 {
		fmt.Printf("Requires:  %s\n", strings.Join(portNames(comp.Requires), ", "))
	}
	if len(comp.Slots) > 0 {
		fmt.Println("Slots:")
		for _, s := range comp.Slots {
			fmt.Printf("  - %s (proto: %s, default: %s)\n", s.Name, s.Proto, s.Default)
		}
	}
	if len(comp.Config) > 0 {
		fmt.Println("Config:")
		for key, field := range comp.Config {
			if field.Default != nil {
				fmt.Printf("  %s: type=%s, default=%v\n", key, field.Type, field.Default)
			} else {
				fmt.Printf("  %s: type=%s\n", key, field.Type)
			}
		}
	}
	return nil
}

// runRegistryFills implements `stego registry fills --slot <s>`.
func runRegistryFills(args []string) error {
	fs := flag.NewFlagSet("registry fills", flag.ContinueOnError)
	slotName := fs.String("slot", "", "slot name to search fills for (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *slotName == "" {
		fs.Usage()
		return fmt.Errorf("--slot is required")
	}

	projectDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting working directory: %w", err)
	}

	// Find fills in the project's fills/ directory.
	fillsDir := filepath.Join(projectDir, "fills")
	if _, err := os.Stat(fillsDir); err != nil {
		fmt.Println("No fills directory found.")
		return nil
	}

	entries, err := os.ReadDir(fillsDir)
	if err != nil {
		return fmt.Errorf("reading fills directory: %w", err)
	}

	found := false
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		fillYAML := filepath.Join(fillsDir, entry.Name(), "fill.yaml")
		fill, err := parser.ParseFill(fillYAML)
		if err != nil {
			continue
		}

		// Match on the slot portion of the implements field (component.slot).
		parts := strings.SplitN(fill.Implements, ".", 2)
		implementsSlot := fill.Implements
		if len(parts) == 2 {
			implementsSlot = parts[1]
		}

		if implementsSlot == *slotName {
			if !found {
				fmt.Printf("Fills implementing slot %q:\n", *slotName)
				found = true
			}
			qualifiedInfo := ""
			if fill.QualifiedBy != "" {
				qualifiedInfo = fmt.Sprintf(" (qualified by %s at %s)", fill.QualifiedBy, fill.QualifiedAt.Format(time.DateOnly))
			}
			fmt.Printf("  %s%s\n", fill.Name, qualifiedInfo)
		}
	}

	if !found {
		fmt.Printf("No fills found for slot %q\n", *slotName)
	}
	return nil
}

// buildReconcilerInput creates a ReconcilerInput from environment and filesystem.
func buildReconcilerInput() (compiler.ReconcilerInput, error) {
	projectDir, err := os.Getwd()
	if err != nil {
		return compiler.ReconcilerInput{}, fmt.Errorf("getting working directory: %w", err)
	}

	registryDir := resolveRegistryDir(projectDir)
	if _, err := os.Stat(registryDir); err != nil {
		return compiler.ReconcilerInput{}, fmt.Errorf("registry directory not found at %s: %w", registryDir, err)
	}

	moduleName := os.Getenv("STEGO_MODULE")
	if moduleName == "" {
		moduleName = "github.com/example/service"
	}

	goVersion := os.Getenv("STEGO_GO_VERSION")
	if goVersion == "" {
		goVersion = "1.22"
	}

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

// resolveRegistryDir returns the registry directory path.
func resolveRegistryDir(projectDir string) string {
	if envReg := os.Getenv("STEGO_REGISTRY"); envReg != "" {
		return envReg
	}
	return filepath.Join(projectDir, "registry")
}

// loadRegistry loads the registry from the standard location.
func loadRegistry(projectDir string) (*registry.Registry, error) {
	registryDir := resolveRegistryDir(projectDir)
	return registry.Load(registryDir)
}

// portListContains checks if a port list contains a port with the given name.
func portListContains(ports []types.Port, name string) bool {
	for _, p := range ports {
		if p.Name == name {
			return true
		}
	}
	return false
}

// slotListContains checks if a slot definition list contains a slot with the given name.
func slotListContains(slots []types.SlotDefinition, name string) bool {
	for _, s := range slots {
		if s.Name == name {
			return true
		}
	}
	return false
}

// portNames returns the names from a port list.
func portNames(ports []types.Port) []string {
	names := make([]string, len(ports))
	for i, p := range ports {
		names[i] = p.Name
	}
	return names
}

// slotDefNames returns the names from a slot definition list.
func slotDefNames(slots []types.SlotDefinition) []string {
	names := make([]string, len(slots))
	for i, s := range slots {
		names[i] = s.Name
	}
	return names
}
