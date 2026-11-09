package modes

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"sync"
)

// Mode is the name of a mode of the binary.
type Mode string

// The modes of a binary. All selects the three others; it is the default and it is allowed in production.
const (
	API        Mode = "api"
	Worker     Mode = "worker"
	Controller Mode = "controller"
	All        Mode = "all"
)

// EnvironmentName is the environment value that holds a comma-separated list of modes when no mode option is
// given.
const EnvironmentName = "STEGO_MODE"

// OptionName is the name of the mode option, which may be given more than once.
const OptionName = "mode"

// order is the order of the modes that a selection runs.
var order = []Mode{API, Worker, Controller}

func known(m Mode) bool {
	return m == API || m == Worker || m == Controller || m == All
}

// Selection is the modes the binary runs, in the order they were given, each once.
type Selection []Mode

// Has tells if the selection runs the mode m, also through all.
func (s Selection) Has(m Mode) bool {
	for _, selected := range s {
		if selected == m || selected == All {
			return true
		}
	}
	return false
}

// Modes is the modes the selection runs, with all expanded, in the order api, worker, controller.
func (s Selection) Modes() []Mode {
	var modes []Mode
	for _, m := range order {
		if s.Has(m) {
			modes = append(modes, m)
		}
	}
	return modes
}

// optionValues is the flag value of the mode option: each use adds one mode.
type optionValues []string

func (o *optionValues) String() string { return strings.Join(*o, ",") }

func (o *optionValues) Set(value string) error {
	*o = append(*o, value)
	return nil
}

// Parse reads the modes from the arguments and from the value of STEGO_MODE that getenv gives (ST-SUP-RP09-LC01).
// The mode option --mode may be given more than once; when any is given, Parse uses the options alone and ignores
// STEGO_MODE. With no option, it reads STEGO_MODE as a comma-separated list, with spaces around a name allowed.
// With neither, the selection is all. It refuses an unknown mode, and an empty one, and names it.
func Parse(args []string, getenv func(string) string) (Selection, error) {
	set := flag.NewFlagSet("modes", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	var options optionValues
	set.Var(&options, OptionName, "a mode to run: api, worker, controller or all; may be given more than once")
	if err := set.Parse(args); err != nil {
		return nil, fmt.Errorf("modes: %w", err)
	}
	if set.NArg() > 0 {
		return nil, fmt.Errorf("modes: unexpected argument %q", set.Arg(0))
	}
	names := []string(options)
	source := "--" + OptionName
	if len(names) == 0 && getenv != nil {
		if value := getenv(EnvironmentName); strings.TrimSpace(value) != "" {
			names = strings.Split(value, ",")
			source = EnvironmentName
		}
	}
	if len(names) == 0 {
		return Selection{All}, nil
	}
	var selection Selection
	for _, name := range names {
		m := Mode(strings.TrimSpace(name))
		if !known(m) {
			return nil, fmt.Errorf("modes: unknown mode %q in %s; the modes are api, worker, controller and all", m, source)
		}
		if !contains(selection, m) {
			selection = append(selection, m)
		}
	}
	return selection, nil
}

func contains(s Selection, m Mode) bool {
	for _, selected := range s {
		if selected == m {
			return true
		}
	}
	return false
}

// Component is one part of the binary that a mode runs, such as the HTTP server or the outbox worker. Run runs it
// until ctx ends.
type Component struct {
	Name string
	Run  func(ctx context.Context) error
}

// Registry maps each mode to the component set that the generated code registers for it. It holds no fixed
// bundle: a mode runs only what was registered for it.
type Registry struct {
	mu   sync.Mutex
	sets map[Mode][]Component
}

// NewRegistry returns a registry with no component.
func NewRegistry() *Registry {
	return &Registry{sets: make(map[Mode][]Component)}
}

// Register sets the component set of the mode m, in place of the set registered before. It refuses all, an
// unknown mode, and a component with no name or no run function.
func (r *Registry) Register(m Mode, components ...Component) error {
	if m == All || !known(m) {
		return fmt.Errorf("modes: cannot register components for the mode %q; register api, worker or controller", m)
	}
	for _, c := range components {
		if c.Name == "" || c.Run == nil {
			return fmt.Errorf("modes: a component of the mode %s needs a name and a run function", m)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sets[m] = append([]Component(nil), components...)
	return nil
}

// Components is the components the selection runs: the sets of its modes in the order api, worker, controller,
// each component name once.
func (r *Registry) Components(selection Selection) []Component {
	r.mu.Lock()
	defer r.mu.Unlock()
	var components []Component
	seen := make(map[string]bool)
	for _, m := range selection.Modes() {
		for _, c := range r.sets[m] {
			if !seen[c.Name] {
				seen[c.Name] = true
				components = append(components, c)
			}
		}
	}
	return components
}

// Running is the components that Start started.
type Running struct {
	wg   sync.WaitGroup
	mu   sync.Mutex
	errs []error
}

// Start runs each component of the selection in its own goroutine with ctx. It refuses a selection with an
// unknown mode and starts nothing then.
func (r *Registry) Start(ctx context.Context, selection Selection) (*Running, error) {
	for _, m := range selection {
		if !known(m) {
			return nil, fmt.Errorf("modes: unknown mode %q", m)
		}
	}
	running := &Running{}
	for _, c := range r.Components(selection) {
		running.wg.Add(1)
		go func(c Component) {
			defer running.wg.Done()
			if err := c.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				running.mu.Lock()
				running.errs = append(running.errs, fmt.Errorf("%s: %w", c.Name, err))
				running.mu.Unlock()
			}
		}(c)
	}
	return running, nil
}

// Wait waits for every started component to return, and joins their errors.
func (r *Running) Wait() error {
	r.wg.Wait()
	r.mu.Lock()
	defer r.mu.Unlock()
	return errors.Join(r.errs...)
}
