package main

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jsell-rh/stego/internal/compiler"
	"github.com/jsell-rh/stego/internal/lifecycle"
	"github.com/jsell-rh/stego/internal/registry"
)

// pinReport is the standing of each pin of one run, as validate, plan and apply show it (ST-DIS-QRY03), with the
// pins that do not stand.
type pinReport struct {
	states []lifecycle.PinState
	// held tells, for each pin, whether the record already holds it.
	held map[lifecycle.Subject]bool
}

// readPinStanding gives the standing of the component pins that the run takes, read from the lifecycle records at
// the head of the service's common registry, not from the snapshot the service pins (ST-DIS-UC05). The pins are
// the components of the plan with their component digests; a pin is held when the record already names it.
func readPinStanding(input compiler.ReconcilerInput, plan *compiler.Plan, now time.Time) pinReport {
	report := pinReport{held: map[lifecycle.Subject]bool{}}
	var pins []lifecycle.Subject
	if plan != nil && plan.NewState != nil && plan.NewState.LastApplied != nil {
		for name, pin := range plan.NewState.LastApplied.Components {
			pins = append(pins, lifecycle.Subject{Kind: lifecycle.ComponentPackage, Name: name, Digest: pin.SHA})
		}
	}
	sort.Slice(pins, func(i, j int) bool { return pins[i].Name < pins[j].Name })
	if state, err := compiler.LoadState(filepath.Join(input.ProjectDir, ".stego", "state.yaml")); err == nil &&
		state.FormatVersion >= 2 && state.LastApplied != nil {
		for name, pin := range state.LastApplied.Components {
			report.held[lifecycle.Subject{Kind: lifecycle.ComponentPackage, Name: name, Digest: pin.SHA}] = true
		}
	}
	lifecycles, err := headLifecycles(input.ProjectDir)
	report.states = lifecycle.PinStanding(lifecycle.NewStandingReader(lifecycle.Project(pins, lifecycles, err, now)))
	return report
}

// problems are the pins that do not stand: past the fail date, a new pin to a withdrawn digest, or a standing
// that could not be read.
func (r pinReport) problems() []string {
	var problems []string
	for _, state := range r.states {
		switch {
		case state.Unchecked:
			return []string{"could not check the standing of the pins: " + state.Cause}
		case state.PastFailDate:
			problems = append(problems, describePin(state)+" is past its fail date")
		case state.Withdrawn && !r.held[state.Pin]:
			problems = append(problems, describePin(state)+" is a new pin to a withdrawn digest")
		}
	}
	return problems
}

func describePin(state lifecycle.PinState) string {
	text := state.Pin.Name + " " + state.Pin.Digest
	if s := state.Supersession; s != nil {
		text += " (superseded by " + s.Successor.Digest + ", fails after " + lifecycle.FormatDate(s.FailsAfter) + ")"
	}
	if w := state.Withdrawal; w != nil {
		text += " (withdrawn: " + w.Reason + "; fails after " + lifecycle.FormatDate(w.FailsAfter) + ")"
	}
	return text
}

// write prints the standing of each pin. When the standing could not be read, it says so, names the cause and
// reports no pin as current.
func (r pinReport) write(out io.Writer) {
	fmt.Fprintln(out, "Pin standing:")
	for _, state := range r.states {
		if state.Unchecked {
			fmt.Fprintf(out, "  Could not check the standing of the pins: %s. No pin is reported as current.\n", state.Cause)
			return
		}
	}
	for _, state := range r.states {
		pin := state.Pin.Name + " " + state.Pin.Digest
		if state.Current() {
			fmt.Fprintf(out, "  current %s\n", pin)
			continue
		}
		past := ""
		if state.PastFailDate {
			past = " PAST ITS FAIL DATE"
		}
		if s := state.Supersession; s != nil {
			line := fmt.Sprintf("  superseded%s %s by %s, fails after %s", past, pin, s.Successor.Digest, lifecycle.FormatDate(s.FailsAfter))
			if s.Successor.Name != state.Pin.Name {
				line += " (successor component " + s.Successor.Name + ")"
			}
			if s.NeedsDeclarationChange {
				line += "; taking it needs a declaration change"
			}
			if s.NeedsFillChange {
				line += "; taking it needs a fill change"
			}
			fmt.Fprintln(out, line)
		}
		if w := state.Withdrawal; w != nil {
			fmt.Fprintf(out, "  withdrawn%s %s: %s; fails after %s\n", past, pin, w.Reason, lifecycle.FormatDate(w.FailsAfter))
		}
	}
}

// headLifecycles reads the lifecycle records of the service's common registry at its current head: the folder
// itself for a local registry or the STEGO_REGISTRY override, and the tip of the repository's HEAD, fetched with
// git, for a pinned Git registry.
func headLifecycles(projectDir string) ([]lifecycle.Lifecycle, error) {
	if dir := os.Getenv("STEGO_REGISTRY"); dir != "" {
		return lifecycle.NewStore(dir).All()
	}
	config, err := registry.LoadConfig(filepath.Join(projectDir, ".stego", "config.yaml"))
	if err != nil {
		return nil, err
	}
	if len(config.Registry) == 0 {
		return nil, errors.New("the project names no registry")
	}
	source := config.Registry[0]
	if source.Vendor == "" && !strings.Contains(source.URL, "://") && !strings.HasPrefix(source.URL, "git@") {
		dir := source.URL
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(projectDir, dir)
		}
		if source.Path != "" {
			dir = filepath.Join(dir, filepath.FromSlash(source.Path))
		}
		return lifecycle.NewStore(dir).All()
	}
	if source.Vendor != "" {
		return nil, errors.New("a vendored registry has no head to read the lifecycle records from")
	}
	return gitHeadLifecycles(source.URL, source.Path)
}

// gitHeadLifecycles fetches the head commit of the registry repository into a temporary repository and reads its
// lifecycle folder from it.
func gitHeadLifecycles(url, subfolder string) ([]lifecycle.Lifecycle, error) {
	if strings.HasPrefix(url, "-") {
		return nil, fmt.Errorf("the registry address %q starts with \"-\", which git would read as an option", url)
	}
	temporary, err := os.MkdirTemp("", "stego-lifecycle-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(temporary)
	repository := filepath.Join(temporary, "head.git")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	git := func(args ...string) ([]byte, error) {
		command := exec.CommandContext(ctx, "git", args...)
		command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1")
		var stderr bytes.Buffer
		command.Stderr = &stderr
		output, err := command.Output()
		if err != nil {
			return nil, fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(stderr.String()))
		}
		return output, nil
	}
	if _, err := git("init", "-q", "--bare", repository); err != nil {
		return nil, err
	}
	// "--" ends the options, so the address is never read as an option of git; an address that starts with "-" is
	// refused before git runs.
	if _, err := git("-C", repository, "fetch", "-q", "--depth", "1", "--no-tags", "--", url, "HEAD"); err != nil {
		return nil, fmt.Errorf("fetching the head of the registry %s: %w", url, err)
	}
	folder := lifecycle.Folder
	if subfolder != "" {
		folder = path.Join(subfolder, lifecycle.Folder)
	}
	extract := filepath.Join(temporary, "head")
	store := lifecycle.NewStore(extract)
	listing, err := git("-C", repository, "ls-tree", "--name-only", "FETCH_HEAD", folder+"/")
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(listing)) == 0 {
		return nil, fmt.Errorf("%w at the head of the registry %s", lifecycle.ErrNoFolder, url)
	}
	archive, err := git("-C", repository, "archive", "--format=tar", "FETCH_HEAD", folder)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(store.Dir(), 0o700); err != nil {
		return nil, err
	}
	reader := tar.NewReader(bytes.NewReader(archive))
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if header.Typeflag == tar.TypeDir || header.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		name := path.Base(header.Name)
		if path.Dir(header.Name) != folder || header.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("the lifecycle folder at the head of the registry holds %s, which is not an advisory file", header.Name)
		}
		data, err := io.ReadAll(io.LimitReader(reader, 1<<20+1))
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(store.Dir(), name), data, 0o600); err != nil {
			return nil, err
		}
	}
	return store.All()
}

// checkPinStanding prints the standing of each pin of the run to out, and refuses the run when a pin does not stand:
// past its fail date, a new pin to a withdrawn digest, or a standing that could not be read (ST-DIS-UC05-R03,
// ST-DIS-UC05-R04, ST-DIS-UC07-R03). A superseded pin before its fail date is reported and does not fail the run
// (ST-DIS-UC05-R01).
func checkPinStanding(input compiler.ReconcilerInput, plan *compiler.Plan, out io.Writer) error {
	report := readPinStanding(input, plan, time.Now())
	report.write(out)
	if problems := report.problems(); len(problems) > 0 {
		return fmt.Errorf("a pin does not stand: %s", strings.Join(problems, "; "))
	}
	return nil
}
