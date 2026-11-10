package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jsell-rh/stego/internal/buildidentity"
	"github.com/jsell-rh/stego/internal/compiler"
	"github.com/jsell-rh/stego/internal/declaration"
	"github.com/jsell-rh/stego/internal/fill"
	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/auditlog"
	"github.com/jsell-rh/stego/internal/generator/browserbackend"
	"github.com/jsell-rh/stego/internal/generator/browsertelemetry"
	"github.com/jsell-rh/stego/internal/generator/cliapplication"
	"github.com/jsell-rh/stego/internal/generator/controller"
	"github.com/jsell-rh/stego/internal/generator/gosdk"
	"github.com/jsell-rh/stego/internal/generator/grpcapplication"
	"github.com/jsell-rh/stego/internal/generator/healthcheck"
	"github.com/jsell-rh/stego/internal/generator/httpapplication"
	"github.com/jsell-rh/stego/internal/generator/jwtauth"
	"github.com/jsell-rh/stego/internal/generator/kafkaproducer"
	"github.com/jsell-rh/stego/internal/generator/keycloakprovider"
	"github.com/jsell-rh/stego/internal/generator/kubernetesclient"
	"github.com/jsell-rh/stego/internal/generator/kubernetesservice"
	"github.com/jsell-rh/stego/internal/generator/kubernetesworkload"
	"github.com/jsell-rh/stego/internal/generator/oteltracing"
	"github.com/jsell-rh/stego/internal/generator/outbox"
	"github.com/jsell-rh/stego/internal/generator/postgresadapter"
	"github.com/jsell-rh/stego/internal/generator/postgresclient"
	"github.com/jsell-rh/stego/internal/generator/restapi"
	"github.com/jsell-rh/stego/internal/generator/rhssoauth"
	"github.com/jsell-rh/stego/internal/generator/runtimeconfig"
	"github.com/jsell-rh/stego/internal/generator/tslsearch"
	"github.com/jsell-rh/stego/internal/generator/typescriptsdk"
	"github.com/jsell-rh/stego/internal/registry"
	"github.com/jsell-rh/stego/internal/slot"
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
	case "image":
		err = runImage(os.Args[2:])
	case "build":
		err = runBuild(os.Args[2:])
	case "assets":
		err = runAssets(os.Args[2:])
	case "version":
		err = runVersion(os.Args[2:], os.Stdout)
	case "init":
		err = runInit(os.Args[2:])
	case "plan":
		err = runPlan(os.Args[2:])
	case "apply":
		err = runApply(os.Args[2:])
	case "recover":
		err = runRecover(os.Args[2:])
	case "deps":
		err = runDependencies(os.Args[2:])
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
	case "lifecycle":
		err = runLifecycle(os.Args[2:])
	case "fleet":
		err = runFleet(os.Args[2:])
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
	fmt.Fprintln(os.Stderr, "  fill create     Create a fill with shared slot contracts")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Reconciliation:")
	fmt.Fprintln(os.Stderr, "  plan            Diff desired vs current, show changeset")
	fmt.Fprintln(os.Stderr, "  apply           Generate/update code")
	fmt.Fprintln(os.Stderr, "  recover         Complete an interrupted apply")
	fmt.Fprintln(os.Stderr, "  assets          Pack a checked browser asset bundle")
	fmt.Fprintln(os.Stderr, "  image           Build and verify static Go application images")
	fmt.Fprintln(os.Stderr, "  build           Build and verify recorded application artifacts")
	fmt.Fprintln(os.Stderr, "  deps            Resolve and check project dependencies")
	fmt.Fprintln(os.Stderr, "  drift           Detect hand-edits to generated files")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Validation:")
	fmt.Fprintln(os.Stderr, "  validate        Check service.yaml against registry")
	fmt.Fprintln(os.Stderr, "  test            Run all fill tests and record their evidence")
	fmt.Fprintln(os.Stderr, "  fill sign       Sign a fill's evidence with your key")
	fmt.Fprintln(os.Stderr, "  fill status     Show the qualification of each bound fill")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Registry:")
	fmt.Fprintln(os.Stderr, "  registry search     Query components by provides/requires/slots")
	fmt.Fprintln(os.Stderr, "  registry inspect    Show component details")
	fmt.Fprintln(os.Stderr, "  registry fills      Find existing fills for a slot")
	fmt.Fprintln(os.Stderr, "  lifecycle supersede Record the successor of a digest, with its fail date")
	fmt.Fprintln(os.Stderr, "  lifecycle withdraw  Record that a digest must not be used, with its fail date")
	fmt.Fprintln(os.Stderr, "  fleet               Name every service that pins a component digest")
	fmt.Fprintln(os.Stderr, "  fleet vex           Print an OpenVEX statement per service and advisory")
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

	if fs.NArg() != 0 {
		return fmt.Errorf("init does not accept positional arguments")
	}
	projectDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting working directory: %w", err)
	}
	result, err := compiler.Initialize(compiler.InitOptions{ProjectDir: projectDir, Archetype: *archetype, Stderr: os.Stderr})
	if err != nil {
		return err
	}
	fmt.Printf("Initialized stego project %q with archetype %q\n", result.Name, *archetype)
	fmt.Println("Created:")
	for _, path := range result.Created {
		fmt.Printf("  %s\n", path)
	}
	return nil
}

// runFill dispatches fill subcommands.
func runFill(args []string) error {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: stego fill <subcommand>")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Subcommands:")
		fmt.Fprintln(os.Stderr, "  create    Scaffold a new fill with generated interface")
		fmt.Fprintln(os.Stderr, "  sign      Sign a fill's test evidence with your key")
		fmt.Fprintln(os.Stderr, "  status    Show the qualification of each bound fill")
		return fmt.Errorf("fill subcommand required")
	}
	switch args[0] {
	case "create":
		return runFillCreate(args[1:])
	case "sign":
		return runFillSign(args[1:])
	case "status":
		return runFillStatus(args[1:])
	case "--help", "-h":
		fmt.Fprintln(os.Stderr, "Usage: stego fill <subcommand>")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Subcommands:")
		fmt.Fprintln(os.Stderr, "  create    Scaffold a new fill with generated interface")
		fmt.Fprintln(os.Stderr, "  sign      Sign a fill's test evidence with your key")
		fmt.Fprintln(os.Stderr, "  status    Show the qualification of each bound fill")
		return nil
	default:
		return fmt.Errorf("unknown fill subcommand: %s", args[0])
	}
}

// runFillCreate implements `stego fill create <name> --slot <s> [--collection <c>]`.
// Supports both `fill create <name> --slot <s>` and `fill create --slot <s> <name>`.
func runFillCreate(args []string) error {
	fs := flag.NewFlagSet("fill create", flag.ContinueOnError)
	slotName := fs.String("slot", "", "slot to implement (required)")
	componentName := fs.String("component", "", "component that owns the slot (required when slot name is ambiguous)")
	collectionName := fs.String("collection", "", "collection this fill is scoped to")

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

	if len(positional) != 1 {
		fs.Usage()
		return fmt.Errorf("fill name is required")
	}
	fillName := positional[0]
	packageName, err := fillPackageName(fillName)
	if err != nil {
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

	// Load registry to find the slot's proto definition.
	result, err := registry.ResolveRegistry(registry.ResolveOptions{ProjectDir: projectDir})
	if err != nil {
		return err
	}
	reg, err := registry.LoadDirectories(result.Dirs...)
	if err != nil {
		return err
	}

	// Find which component(s) own this slot. Multiple components can define
	// slots with the same name (spec: "duplication is cheaper than coupling").
	var matchingComps []*registry.Component
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

	var ownerComp *registry.Component
	if len(matchingComps) > 1 {
		if *componentName == "" {
			names := make([]string, len(matchingComps))
			for i, c := range matchingComps {
				names[i] = c.Name
			}
			sort.Strings(names)
			return fmt.Errorf("slot %q is defined by multiple components: %s — specify --component to disambiguate", *slotName, strings.Join(names, ", "))
		}
		for _, c := range matchingComps {
			if c.Name == *componentName {
				ownerComp = c
				break
			}
		}
		if ownerComp == nil {
			return fmt.Errorf("component %q does not define slot %q", *componentName, *slotName)
		}
	} else {
		ownerComp = matchingComps[0]
		// Validate --component even when unambiguous: if the user explicitly
		// specified a component, verify it actually defines the slot.
		if *componentName != "" && *componentName != ownerComp.Name {
			return fmt.Errorf("component %q does not define slot %q (slot is defined by %q)", *componentName, *slotName, ownerComp.Name)
		}
	}

	// Build the scaffold before creating application files.
	for _, name := range []string{ownerComp.Name, *slotName} {
		if err := gen.ValidatePath(name); err != nil || strings.Contains(name, "/") {
			return fmt.Errorf("invalid component or slot path %q", name)
		}
	}
	protoPath := filepath.ToSlash(filepath.Join("components", ownerComp.Name, "slots", *slotName+".proto"))
	protoData, err := reg.ReadFile(protoPath)
	if err != nil {
		return fmt.Errorf("opening slot contract: %w", err)
	}
	proto, err := slot.ParseProto(bytes.NewReader(protoData))
	if err != nil {
		return fmt.Errorf("reading slot contract: %w", err)
	}
	moduleName, _, err := compiler.ProjectModuleSettings(projectDir, os.Getenv("STEGO_MODULE"), os.Getenv("STEGO_GO_VERSION"))
	if err != nil {
		return err
	}
	source, err := slot.GenerateFill(packageName, moduleName+"/out/slots", proto)
	if err != nil {
		return err
	}
	manifest := fill.Fill{
		Kind: "fill", Name: fillName,
		Implements: ownerComp.Name + "." + *slotName,
		Collection: *collectionName,
	}
	fillData, err := yaml.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("encoding fill.yaml: %w", err)
	}
	if err := reg.Verify(); err != nil {
		return err
	}
	if err := writeFillScaffold(projectDir, fillName, fillData, source); err != nil {
		return err
	}
	fmt.Printf("Created fill %q for slot %q\n", fillName, *slotName)
	fmt.Printf("  fills/%s/fill.yaml\n  fills/%s/fill.go\n", fillName, fillName)
	fmt.Println("Run stego apply to create the slot contracts, write the fill's tests, run stego test to record its evidence and stego fill sign to sign it, then bind the fill in service.yaml.")
	fmt.Println("Implement the fill methods before use. Unfinished methods return an error.")
	return nil
}

// runPlan implements `stego plan`.
func runPlan(args []string) error {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	show := fs.Bool("show", false, "print the saved plan and whether it still applies, and save no plan")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("plan takes no arguments")
	}

	input, err := buildReconcilerInput()
	if err != nil {
		return err
	}

	if *show {
		saved, err := compiler.LoadPlan(input.ProjectDir)
		if err != nil {
			return err
		}
		fmt.Print(compiler.FormatSavedPlan(saved))
		plan, err := compiler.Reconcile(input)
		if err == nil {
			err = saved.StillApplies(plan)
		}
		if err != nil {
			fmt.Printf("The saved plan does not apply: %v\n", err)
			return nil
		}
		fmt.Println("The saved plan still applies.")
		return nil
	}

	plan, err := compiler.Reconcile(input)
	if err != nil {
		return err
	}
	if err := checkPinStanding(input, plan); err != nil {
		return err
	}
	if err := compiler.SavePlan(plan, input.ProjectDir); err != nil {
		return err
	}

	fmt.Print(compiler.FormatPlan(plan))
	fmt.Println("\nSaved the plan in .stego/plan.yaml; stego apply --saved-plan carries it out.")
	return nil
}

// runApply implements `stego apply`.
func runApply(args []string) error {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	replaceHandEdits := fs.Bool("replace-hand-edits", false, "let this apply replace generated files that were edited by hand")
	savedPlan := fs.Bool("saved-plan", false, "carry out the plan that stego plan saved, or refuse it when it is stale")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("apply takes no arguments")
	}

	input, err := buildReconcilerInput()
	if err != nil {
		return err
	}

	plan, err := compiler.Reconcile(input)
	if err != nil {
		return err
	}

	if err := checkPinStanding(input, plan); err != nil {
		return err
	}
	if *savedPlan {
		saved, err := compiler.LoadPlan(input.ProjectDir)
		if err != nil {
			return err
		}
		plan.UseSavedPlan(saved)
	} else if !plan.HasChanges() {
		fmt.Println("No changes. Infrastructure is up-to-date.")
		return nil
	}

	plan.ReplaceHandEdits = *replaceHandEdits
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
	structured := fs.Bool("json", false, "print the faults as one JSON document")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("validate does not accept positional arguments")
	}

	input, err := buildReconcilerInput()
	if err != nil {
		return err
	}

	result, err := compiler.Validate(input)
	if err != nil {
		return err
	}

	faults := declaration.NewFaultsReader(result)
	if *structured {
		document, err := faults.JSON()
		if err != nil {
			return err
		}
		fmt.Println(string(document))
	} else {
		fmt.Print(faults.Prose())
	}
	if result.HasErrors() {
		return fmt.Errorf("validation failed with %d error(s)", len(result.Errors))
	}
	plan, err := compiler.Reconcile(input)
	if err != nil {
		return err
	}
	return checkPinStanding(input, plan)
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
		return fmt.Errorf("drift detected in %d file(s) and %d input(s)", len(result.Modified)+len(result.Deleted), len(result.Inputs))
	}
	return nil
}

// runTest implements `stego test` (ST-EXT-CMD02): it runs the tests of each
// fill and records the evidence of each fill whose tests pass in its fill
// manifest. Arguments after -- go to go test. It names each fill for which
// it records no evidence, and why, and exits non-zero when a test fails.
func runTest(args []string) error {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	testArgs := fs.Args()
	input, err := buildReconcilerInput()
	if err != nil {
		return err
	}
	check, err := compiler.ResolveFillCheck(input)
	if err != nil {
		return err
	}
	run := func(dir string) error {
		relative, err := filepath.Rel(input.ProjectDir, dir)
		if err != nil {
			return err
		}
		goArgs := append([]string{"test", "./" + filepath.ToSlash(relative) + "/..."}, testArgs...)
		cmd := exec.Command("go", goArgs...)
		cmd.Dir = input.ProjectDir
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}
	results, err := fill.RecordEvidence(check, run, buildidentity.Current(), time.Now)
	if err != nil {
		return err
	}
	var failed []string
	for _, result := range results {
		switch {
		case result.Kept:
			fmt.Printf("fill %s: tests passed; its evidence is current and stays as it is\n", result.Fill)
		case result.Passed != nil:
			fmt.Printf("fill %s: tests passed; evidence recorded (fill digest %s, contract digest %s)\n", result.Fill, result.Passed.FillDigest, result.Passed.ContractDigest)
		default:
			fmt.Printf("fill %s: no evidence recorded: %s\n", result.Fill, result.Reason)
		}
		if result.Failed {
			failed = append(failed, result.Fill)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("the tests of %d fill(s) failed: %s", len(failed), strings.Join(failed, ", "))
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
	case "compose":
		return runRegistryCompose(args[1:], os.Stdout, os.Stderr)
	case "consumers":
		return runRegistryConsumers(args[1:], os.Stdout, os.Stderr)
	case "sunsets":
		return runRegistrySunsets(args[1:], os.Stdout, os.Stderr, time.Now())
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
	fmt.Fprintln(os.Stderr, "  compose    Compose the pinned registry sources into the active set")
	fmt.Fprintln(os.Stderr, "  consumers  List the recorded services that use a component")
	fmt.Fprintln(os.Stderr, "  sunsets    List the recorded services that use a component past its sunset")
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

	result, err := registry.ResolveRegistry(registry.ResolveOptions{ProjectDir: projectDir})
	if err != nil {
		return err
	}
	reg, err := registry.LoadSources(result.Sources...)
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "NAME\tVERSION\tSTAGE\tSOURCE\tPROVIDES\tREQUIRES\tSLOTS\n")

	// The trust of the source that supplies each component.
	sourceOf := map[string]registry.Trust{}
	for _, source := range reg.Sources() {
		for _, part := range source.Parts {
			sourceOf[part] = source.Trust
		}
	}

	components := reg.Components()
	compNames := make([]string, 0, len(components))
	for name := range components {
		compNames = append(compNames, name)
	}
	sort.Strings(compNames)

	for _, name := range compNames {
		comp := components[name]
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

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			name, comp.Version, comp.Stage, sourceOf["component "+name],
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
	archetype := fs.String("archetype", "", "report every input a declaration may give the archetype's active component set")
	structured := fs.Bool("json", false, "print the report as one JSON document")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *archetype != "" && fs.NArg() != 0 {
		return fmt.Errorf("give a component name or --archetype, not both")
	}
	if *archetype == "" && fs.NArg() < 1 {
		return fmt.Errorf("component name is required")
	}

	projectDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting working directory: %w", err)
	}

	result, err := registry.ResolveRegistry(registry.ResolveOptions{ProjectDir: projectDir})
	if err != nil {
		return err
	}
	reg, err := registry.LoadDirectories(result.Dirs...)
	if err != nil {
		return err
	}
	if *archetype != "" {
		return reportDeclarableInputs(reg, *archetype, *structured)
	}
	compName := fs.Arg(0)

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
		configKeys := make([]string, 0, len(comp.Config))
		for key := range comp.Config {
			configKeys = append(configKeys, key)
		}
		sort.Strings(configKeys)
		for _, key := range configKeys {
			field := comp.Config[key]
			if field.Default != nil {
				fmt.Printf("  %s: type=%s, default=%v\n", key, field.Type, field.Default)
			} else {
				fmt.Printf("  %s: type=%s\n", key, field.Type)
			}
		}
	}
	return nil
}

// reportDeclarableInputs prints what may be declared for an archetype (ST-SUP-QRY01): every component of its
// active component set as checking reads it, each setting with its type, allowed values, default and whether it is
// optional, and each slot with its contract. With structured it prints only one JSON document.
func reportDeclarableInputs(reg *registry.Registry, archetype string, structured bool) error {
	components, err := compiler.ActiveComponents(reg, archetype)
	if err != nil {
		return err
	}
	inputs := registry.NewInputsReader(archetype, components)
	if structured {
		document, err := inputs.JSON()
		if err != nil {
			return err
		}
		fmt.Println(string(document))
		return nil
	}
	writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "COMPONENT\tVERSION\tSETTING\tTYPE\tALLOWED\tOPTIONAL\tDEFAULT\tSLOT\tCONTRACT")
	for _, row := range inputs.Rows() {
		optional, value := "", ""
		if row.Optional != nil {
			optional = fmt.Sprint(*row.Optional)
		}
		if row.Default != nil {
			value = fmt.Sprint(row.Default)
		}
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", row.Component, row.ComponentVersion, row.Setting, row.SettingType, strings.Join(row.AllowedValues, ","), optional, value, row.Slot, row.SlotContract)
	}
	return writer.Flush()
}

// runRegistryFills implements `stego registry fills --slot <s> [--component <c>]`
// (ST-SUP-CLI06, ST-EXT-QRY01): it lists each fill of the project that
// implements the slot of the active component set, with its collection and
// its qualification standing, and each fill folder whose manifest it cannot
// read. It writes nothing.
func runRegistryFills(args []string) error {
	fs := flag.NewFlagSet("registry fills", flag.ContinueOnError)
	slotName := fs.String("slot", "", "slot name to search fills for (required)")
	componentName := fs.String("component", "", "component that owns the slot (required when more than one component of the active set declares it)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *slotName == "" {
		fs.Usage()
		return fmt.Errorf("--slot is required")
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("registry fills takes no positional arguments")
	}
	input, err := buildReconcilerInput()
	if err != nil {
		return err
	}
	check, err := compiler.ResolveFillCheck(input)
	if err != nil {
		return err
	}
	ref, err := fill.ResolveSlot(check, *slotName, *componentName)
	if err != nil {
		return err
	}
	entries, err := fill.ListCatalog(check, ref)
	if err != nil {
		return err
	}
	readable := 0
	for _, entry := range entries {
		if entry.Unreadable == "" {
			readable++
		}
	}
	if readable == 0 {
		fmt.Printf("No fill implements the slot %s (%s). Run stego fill create <name> --slot %s --component %s --collection <collection> to add one.\n", *slotName, ref, ref.Slot, ref.Component)
	} else {
		fmt.Printf("Fills that implement the slot %s (%s):\n", *slotName, ref)
	}
	if len(entries) == 0 {
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "FILL\tCOLLECTION\tSTANDING\tREASON\n")
	for _, entry := range entries {
		if entry.Unreadable != "" {
			fmt.Fprintf(w, "%s\t-\tunreadable\t%s\n", entry.Fill, entry.Unreadable)
			continue
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", entry.Fill, entry.Collection, entry.Standing, entry.Reason)
	}
	return w.Flush()
}

// buildReconcilerInput creates a ReconcilerInput from environment and filesystem.
func buildReconcilerInput() (compiler.ReconcilerInput, error) {
	projectDir, err := os.Getwd()
	if err != nil {
		return compiler.ReconcilerInput{}, fmt.Errorf("getting working directory: %w", err)
	}

	result, err := registry.ResolveRegistry(registry.ResolveOptions{ProjectDir: projectDir})
	if err != nil {
		return compiler.ReconcilerInput{}, err
	}

	moduleName, goVersion, err := compiler.ProjectModuleSettings(projectDir, os.Getenv("STEGO_MODULE"), os.Getenv("STEGO_GO_VERSION"))
	if err != nil {
		return compiler.ReconcilerInput{}, err
	}

	outDir := filepath.Join(projectDir, "out")

	return compiler.ReconcilerInput{
		ProjectDir:   projectDir,
		RegistryDir:  result.Dir,
		RegistryDirs: result.Dirs,
		Generators:   defaultGenerators(),
		GoVersion:    goVersion,
		ModuleName:   moduleName,
		RegistrySHA:  result.Ref,
		OutDir:       outDir,
		ReadFill:     fill.ReadCollection,
		ReadFillSlot: fill.ReadImplements,
		QualifyFills: fill.QualificationFaults,
	}, nil
}

func defaultGenerators() map[string]gen.Generator {
	return map[string]gen.Generator{
		"rest-api":              &restapi.Generator{},
		"browser-backend":       &browserbackend.Generator{},
		"browser-telemetry":     &browsertelemetry.Generator{},
		"http-application":      &httpapplication.Generator{},
		"go-sdk":                &gosdk.Generator{},
		"typescript-sdk":        &typescriptsdk.Generator{},
		"cli-application":       &cliapplication.Generator{},
		"kubernetes-client":     &kubernetesclient.Generator{},
		"kubernetes-workload":   &kubernetesworkload.Generator{},
		"runtime-configuration": &runtimeconfig.Generator{},
		"keycloak-provider":     &keycloakprovider.Generator{},
		"kubernetes-service":    &kubernetesservice.Generator{},
		"postgres-client":       &postgresclient.Generator{},
		"controller":            &controller.Generator{},
		"grpc-application":      &grpcapplication.Generator{},
		"grpc-processes":        &grpcapplication.ProcessGenerator{},
		"outbox":                &outbox.Generator{},
		"kafka-producer":        &kafkaproducer.Generator{},
		"postgres-adapter":      &postgresadapter.Generator{},
		"tsl-search":            &tslsearch.Generator{},
		"jwt-auth":              &jwtauth.Generator{},
		"rh-sso-auth":           &rhssoauth.Generator{},
		"otel-tracing":          &oteltracing.Generator{},
		"health-check":          &healthcheck.Generator{},
		"audit-log":             &auditlog.Generator{},
	}
}

// portListContains checks if a port list contains a port with the given name.
func portListContains(ports []registry.Port, name string) bool {
	for _, p := range ports {
		if p.Name == name {
			return true
		}
	}
	return false
}

// slotListContains checks if a slot definition list contains a slot with the given name.
func slotListContains(slots []registry.SlotDefinition, name string) bool {
	for _, s := range slots {
		if s.Name == name {
			return true
		}
	}
	return false
}

// portNames returns the names from a port list.
func portNames(ports []registry.Port) []string {
	names := make([]string, len(ports))
	for i, p := range ports {
		names[i] = p.Name
	}
	return names
}

// slotDefNames returns the names from a slot definition list.
func slotDefNames(slots []registry.SlotDefinition) []string {
	names := make([]string, len(slots))
	for i, s := range slots {
		names[i] = s.Name
	}
	return names
}

// runVersion preserves the short version form and supplies a structured record.
func runVersion(args []string, output io.Writer) error {
	if len(args) == 0 {
		_, err := fmt.Fprintln(output, "stego", version)
		return err
	}
	if len(args) != 1 || args[0] != "--json" {
		return fmt.Errorf("usage: stego version [--json]")
	}
	return json.NewEncoder(output).Encode(currentIdentity(version))
}
