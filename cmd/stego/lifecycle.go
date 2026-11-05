package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/jsell-rh/stego/internal/lifecycle"
	"github.com/jsell-rh/stego/internal/registry"
)

// referenceList is a flag that may be given more than once.
type referenceList []string

func (r *referenceList) String() string     { return strings.Join(*r, ",") }
func (r *referenceList) Set(v string) error { *r = append(*r, v); return nil }

// subjectFlags are the flags that name the digest a lifecycle command is about.
type subjectFlags struct {
	registry, component, runtime, digest *string
}

func addSubjectFlags(fs *flag.FlagSet) subjectFlags {
	return subjectFlags{
		registry:  fs.String("registry", "", "the registry checkout whose lifecycle folder gets the advisory"),
		component: fs.String("component", "", "the component whose digest it is"),
		runtime:   fs.String("runtime", "", "the module path of the runtime library whose version it is"),
		digest:    fs.String("digest", "", "the component digest, or the runtime library version"),
	}
}

func (f subjectFlags) subject() (lifecycle.Subject, error) {
	if *f.registry == "" {
		return lifecycle.Subject{}, errors.New("--registry is missing: name the registry checkout")
	}
	switch {
	case *f.component != "" && *f.runtime != "":
		return lifecycle.Subject{}, errors.New("give --component or --runtime, not both")
	case *f.component != "":
		return lifecycle.Subject{Kind: lifecycle.ComponentPackage, Name: *f.component, Digest: *f.digest}, f.needDigest()
	case *f.runtime != "":
		return lifecycle.Subject{Kind: lifecycle.RuntimePackage, Name: *f.runtime, Digest: *f.digest}, f.needDigest()
	}
	return lifecycle.Subject{}, errors.New("--component or --runtime is missing: name what the digest belongs to")
}

func (f subjectFlags) needDigest() error {
	if *f.digest == "" {
		return errors.New("--digest is missing: name the component digest or the runtime version")
	}
	return nil
}

// yesNo reads a statement flag that must be yes or no.
func yesNo(name, value string) (*bool, error) {
	switch value {
	case "yes":
		v := true
		return &v, nil
	case "no":
		v := false
		return &v, nil
	case "":
		return nil, fmt.Errorf("--%s is missing: state yes or no", name)
	}
	return nil, fmt.Errorf("--%s is %q: state yes or no", name, value)
}

// runLifecycle parses stego lifecycle supersede (ST-DIS-CLI01) and stego lifecycle withdraw (ST-DIS-CLI02).
func runLifecycle(args []string) error {
	return runLifecycleAt(args, os.Stdout, time.Now())
}

func runLifecycleAt(args []string, stdout io.Writer, now time.Time) error {
	if len(args) < 1 {
		printLifecycleUsage()
		return errors.New("lifecycle subcommand required")
	}
	switch args[0] {
	case "supersede":
		return runSupersede(args[1:], stdout, now)
	case "withdraw":
		return runWithdraw(args[1:], stdout, now)
	case "--help", "-h":
		printLifecycleUsage()
		return nil
	}
	return fmt.Errorf("unknown lifecycle subcommand: %s", args[0])
}

func printLifecycleUsage() {
	fmt.Fprintln(os.Stderr, "Usage: stego lifecycle <subcommand> [options]")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Subcommands:")
	fmt.Fprintln(os.Stderr, "  supersede  Record the successor of a component digest or runtime version")
	fmt.Fprintln(os.Stderr, "  withdraw   Record that a component digest or runtime version must not be used")
}

func runSupersede(args []string, stdout io.Writer, now time.Time) error {
	fs := flag.NewFlagSet("lifecycle supersede", flag.ContinueOnError)
	subject := addSubjectFlags(fs)
	successor := fs.String("successor", "", "the successor digest or runtime version")
	successorComponent := fs.String("successor-component", "", "the component of the successor, when it is another component")
	failsAfter := fs.String("fails-after", "", "the last day on which a pin to the superseded digest stands (YYYY-MM-DD)")
	declaration := fs.String("needs-declaration-change", "", "yes or no: taking the successor needs a change to the service declaration")
	fill := fs.String("needs-fill-change", "", "yes or no: taking the successor needs a change to a fill")
	summary := fs.String("summary", "", "what the successor changes, in one line")
	var references referenceList
	fs.Var(&references, "reference", "a link to the change, its review or its release notes (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	superseded, err := subject.subject()
	if err != nil {
		return err
	}
	if *successor == "" {
		return errors.New("--successor is missing: name the successor digest")
	}
	if *failsAfter == "" {
		return errors.New("--fails-after is missing: name the date after which the superseded digest fails")
	}
	failDate, err := lifecycle.ParseDate(*failsAfter)
	if err != nil {
		return fmt.Errorf("--fails-after: %w", err)
	}
	needsDeclaration, err := yesNo("needs-declaration-change", *declaration)
	if err != nil {
		return err
	}
	needsFill, err := yesNo("needs-fill-change", *fill)
	if err != nil {
		return err
	}
	if strings.TrimSpace(*summary) == "" {
		return errors.New("--summary is missing: say what the successor changes")
	}
	next := lifecycle.Subject{Kind: superseded.Kind, Name: superseded.Name, Digest: *successor}
	if *successorComponent != "" {
		next.Name = *successorComponent
	}
	facts, err := supersessionFacts(*subject.registry, superseded, next)
	if err != nil {
		return err
	}
	request := lifecycle.SupersedeRequest{
		Superseded: superseded, Successor: next, FailsAfter: &failDate,
		NeedsDeclarationChange: needsDeclaration, NeedsFillChange: needsFill,
		Summary: *summary, References: references, RecordedOn: now.UTC(),
		SupersededStage: facts.supersededStage, SupersededProvides: facts.supersededProvides,
		SuccessorStage: facts.successorStage, SuccessorTookOver: facts.successorTookOver,
		SuccessorReleasedOn: facts.successorReleasedOn,
	}
	event, err := lifecycle.Supersede(lifecycle.NewStore(*subject.registry), request)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Recorded advisory %s: %s is superseded by %s and fails after %s.\n",
		event.Supersession.AdvisoryID, event.Subject, event.Supersession.Successor, lifecycle.FormatDate(event.Supersession.FailsAfter))
	return nil
}

// facts are what the registry states about the two components of a supersession.
type facts struct {
	supersededStage, successorStage string
	supersededProvides              []string
	successorTookOver               []string
	successorReleasedOn             time.Time
}

// supersessionFacts reads from the registry checkout what the rules of a supersession need: the ports each
// component provides. The registry states no component stage and no release record yet (Supplying), so the
// stage and the release date stay unknown, and the window rule refuses the supersession by name.
func supersessionFacts(dir string, superseded, successor lifecycle.Subject) (facts, error) {
	var result facts
	if superseded.Kind != lifecycle.ComponentPackage {
		return result, nil
	}
	reg, err := registry.LoadDirectories(dir)
	if err != nil {
		return result, fmt.Errorf("reading the registry checkout %s: %w", dir, err)
	}
	from := reg.Component(superseded.Name)
	if from == nil {
		return result, fmt.Errorf("the registry has no component %s", superseded.Name)
	}
	for _, port := range from.Provides {
		result.supersededProvides = append(result.supersededProvides, port.Name)
	}
	to := reg.Component(successor.Name)
	if to == nil {
		return result, fmt.Errorf("the registry has no component %s", successor.Name)
	}
	if successor.Name != superseded.Name {
		for _, port := range to.Provides {
			if slices.Contains(result.supersededProvides, port.Name) {
				result.successorTookOver = append(result.successorTookOver, port.Name)
			}
		}
	}
	return result, nil
}

func runWithdraw(args []string, stdout io.Writer, now time.Time) error {
	fs := flag.NewFlagSet("lifecycle withdraw", flag.ContinueOnError)
	subject := addSubjectFlags(fs)
	failsAfter := fs.String("fails-after", "", "the future day after which a pin that a service holds fails (YYYY-MM-DD)")
	reason := fs.String("reason", "", "why the digest must not be used any more")
	var references referenceList
	fs.Var(&references, "reference", "a link to the finding that caused the withdrawal (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	withdrawn, err := subject.subject()
	if err != nil {
		return err
	}
	if *failsAfter == "" {
		return errors.New("--fails-after is missing: a withdrawal carries the future date after which the digest fails")
	}
	failDate, err := lifecycle.ParseDate(*failsAfter)
	if err != nil {
		return fmt.Errorf("--fails-after: %w", err)
	}
	if strings.TrimSpace(*reason) == "" {
		return errors.New("--reason is missing: say why the digest must not be used")
	}
	event, err := lifecycle.Withdraw(lifecycle.NewStore(*subject.registry), lifecycle.WithdrawRequest{
		Withdrawn: withdrawn, FailsAfter: &failDate, Reason: *reason, References: references, RecordedOn: now.UTC(),
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Recorded advisory %s: %s is withdrawn and fails after %s.\n",
		event.Withdrawal.AdvisoryID, event.Subject, lifecycle.FormatDate(event.Withdrawal.FailsAfter))
	return nil
}
