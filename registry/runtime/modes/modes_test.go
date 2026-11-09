package modes

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
)

func env(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func parse(t *testing.T, args []string, stegoMode string) Selection {
	t.Helper()
	selection, err := Parse(args, env(map[string]string{EnvironmentName: stegoMode}))
	if err != nil {
		t.Fatalf("Parse(%v, STEGO_MODE=%q): %v", args, stegoMode, err)
	}
	return selection
}

func expect(t *testing.T, got Selection, want ...Mode) {
	t.Helper()
	if !slices.Equal(got, Selection(want)) {
		t.Fatalf("the selected modes are %v, want %v", got, want)
	}
}

// started records the components a test registry started.
type started struct {
	mu    sync.Mutex
	names []string
}

func (s *started) component(name string) Component {
	return Component{Name: name, Run: func(context.Context) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.names = append(s.names, name)
		return nil
	}}
}

func (s *started) take() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := s.names
	s.names = nil
	slices.Sort(names)
	return names
}

func TestParseModes(t *testing.T) {
	t.Run("ST-SUP-RP09-LC01-T01 The mode option may be given more than once", func(t *testing.T) {
		expect(t, parse(t, []string{"--mode", "api", "--mode", "worker"}, ""), API, Worker)
	})

	t.Run("ST-SUP-RP09-LC01-T02 The mode comes from STEGO_MODE when no option is given", func(t *testing.T) {
		expect(t, parse(t, nil, "worker"), Worker)
	})

	t.Run("ST-SUP-RP09-LC01-T03 The default mode is all", func(t *testing.T) {
		selection, err := Parse(nil, env(nil))
		if err != nil {
			t.Fatal(err)
		}
		expect(t, selection, All)
	})

	t.Run("ST-SUP-RP09-LC01-T04 An unknown mode is refused at startup", func(t *testing.T) {
		s := &started{}
		registry := NewRegistry()
		if err := registry.Register(API, s.component("http")); err != nil {
			t.Fatal(err)
		}
		for _, input := range []struct {
			args []string
			env  string
		}{{[]string{"--mode", "reporter"}, ""}, {nil, "reporter"}} {
			selection, err := Parse(input.args, env(map[string]string{EnvironmentName: input.env}))
			if err == nil || !strings.Contains(err.Error(), "reporter") {
				t.Fatalf("Parse(%v, STEGO_MODE=%q) gave %v, %v, want an error that names reporter", input.args, input.env, selection, err)
			}
		}
		running, err := registry.Start(context.Background(), Selection{"reporter"})
		if err == nil || !strings.Contains(err.Error(), "reporter") || running != nil {
			t.Fatalf("Start of reporter gave %v, %v, want an error that names reporter", running, err)
		}
		if names := s.take(); len(names) != 0 {
			t.Fatalf("components started: %v, want none", names)
		}
	})

	t.Run("ST-SUP-RP09-LC01-T05 A mode runs the component set the generated code registered for it", func(t *testing.T) {
		s := &started{}
		registry := NewRegistry()
		if err := registry.Register(API, s.component("http"), s.component("metrics")); err != nil {
			t.Fatal(err)
		}
		if err := registry.Register(Worker, s.component("outbox")); err != nil {
			t.Fatal(err)
		}
		run := func() []string {
			running, err := registry.Start(context.Background(), parse(t, []string{"--mode", "api"}, ""))
			if err != nil {
				t.Fatal(err)
			}
			if err := running.Wait(); err != nil {
				t.Fatal(err)
			}
			return s.take()
		}
		if got := run(); !slices.Equal(got, []string{"http", "metrics"}) {
			t.Fatalf("the first selection of api started %v, want http and metrics", got)
		}
		if err := registry.Register(API, s.component("http")); err != nil {
			t.Fatal(err)
		}
		if got := run(); !slices.Equal(got, []string{"http"}) {
			t.Fatalf("the second selection of api started %v, want only http", got)
		}
	})

	t.Run("ST-SUP-RP09-LC01-T06 STEGO_MODE holds a comma-separated list of modes", func(t *testing.T) {
		expect(t, parse(t, nil, "api,worker"), API, Worker)
		expect(t, parse(t, nil, "worker, controller"), Worker, Controller)
		s := &started{}
		registry := NewRegistry()
		if err := registry.Register(API, s.component("http")); err != nil {
			t.Fatal(err)
		}
		selection, err := Parse(nil, env(map[string]string{EnvironmentName: "api,reporter"}))
		if err == nil || !strings.Contains(err.Error(), "reporter") || selection != nil {
			t.Fatalf("STEGO_MODE=api,reporter gave %v, %v, want an error that names reporter and no selection", selection, err)
		}
		if names := s.take(); len(names) != 0 {
			t.Fatalf("components started: %v, want none", names)
		}
	})

	t.Run("ST-SUP-RP09-LC01-T07 A mode option overrides STEGO_MODE", func(t *testing.T) {
		expect(t, parse(t, []string{"--mode", "controller"}, "api,worker"), Controller)
		expect(t, parse(t, []string{"--mode", "api", "--mode", "worker"}, "controller"), API, Worker)
	})
}
