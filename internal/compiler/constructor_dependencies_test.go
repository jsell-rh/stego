package compiler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
)

func TestAssembleRejectsAmbiguousConstructorDependencies(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		for _, used := range []bool{false, true} {
			for _, reverse := range []bool{false, true} {
				consumer := &gen.Wiring{Imports: []string{"consumer"}, Constructors: []string{"consumer.NewHandler(runtime)"}}
				if explicit {
					consumer.ConstructorDeps = map[int][]string{0: {"runtime"}}
				}
				if used {
					consumer.Routes = []string{`mux.Handle("/",handler)`}
				}
				producers := []ComponentWiring{{Name: "events", Wiring: &gen.Wiring{Imports: []string{"events"}, Constructors: []string{"events.NewRuntime()"}}}, {Name: "tracing", Wiring: &gen.Wiring{Imports: []string{"tracing"}, Constructors: []string{"tracing.NewRuntime()"}}}}
				if reverse {
					producers[0], producers[1] = producers[1], producers[0]
				}
				input := AssemblerInput{ModuleName: "example.com/test", GoVersion: "1.26.0", Wirings: append(producers, ComponentWiring{Name: "consumer", Wiring: consumer})}
				files, err := Assemble(input)
				if err == nil || len(files) != 0 || !strings.Contains(err.Error(), `ambiguous dependency "runtime" from components events, tracing`) {
					t.Fatalf("ambiguous runtime selected (explicit=%t used=%t reverse=%t): %v", explicit, used, reverse, err)
				}
			}
		}
	}
}
func TestAmbiguousConstructorReferencesUseGoSyntax(t *testing.T) {
	producers := []ComponentWiring{{Name: "first", Wiring: &gen.Wiring{Constructors: []string{"first.NewRuntime()"}}}, {Name: "second", Wiring: &gen.Wiring{Constructors: []string{"second.NewRuntime()"}}}}
	for _, test := range []struct {
		expression string
		reject     bool
	}{
		{`consumer.New("runtime")`, false},
		{`consumer.New(other.runtime)`, false},
		{`consumer.New(runtime.Version())`, false},
		{`consumer.New(struct{runtime string}{runtime:"value"})`, false},
		{`consumer.New(runtime)`, true},
		{`consumer.New(map[any]any{runtime:1})`, true},
		{`consumer.New(map[string]any{"value":runtime})`, true},
		{`consumer.New(struct{value any}{value:runtime})`, true},
		{`consumer.New(func(runtime any) any{return runtime})`, true},
	} {
		t.Run(test.expression, func(t *testing.T) {
			wirings := append(append([]ComponentWiring{}, producers...), ComponentWiring{Name: "consumer", Wiring: &gen.Wiring{Constructors: []string{test.expression}}})
			err := validateConstructorDependencies(wirings, map[string]bool{"runtime": true})
			if (err != nil) != test.reject {
				t.Fatalf("unexpected reference classification: %v", err)
			}
		})
	}
	wirings := append(producers, ComponentWiring{Name: "consumer", Wiring: &gen.Wiring{Constructors: []string{`consumer.New(runtime.Method())`}}})
	if err := validateConstructorDependencies(wirings, nil); err == nil {
		t.Fatal("method receiver selected an ambiguous runtime")
	}
}
func TestReconcileRejectsAmbiguousDependenciesWithoutPlan(t *testing.T) {
	project, registry := setupTestProject(t)
	output := filepath.Join(project, "out", "sentinel.go")
	if err := os.MkdirAll(filepath.Dir(output), 0755); err != nil {
		t.Fatal(err)
	}
	const sentinel = "retained application output\n"
	if err := os.WriteFile(output, []byte(sentinel), 0644); err != nil {
		t.Fatal(err)
	}
	generators := map[string]gen.Generator{
		"stub-api":   &stubGenerator{wiring: &gen.Wiring{Imports: []string{"internal/api"}, Constructors: []string{"api.NewRuntime()", "api.NewHandler(runtime)"}, ConstructorDeps: map[int][]string{1: {"runtime"}}, Routes: []string{`mux.Handle("/",handler)`}}},
		"stub-store": &stubGenerator{wiring: &gen.Wiring{Imports: []string{"internal/storage"}, Constructors: []string{"storage.NewRuntime()"}}},
	}
	plan, err := Reconcile(ReconcilerInput{ProjectDir: project, RegistryDir: registry, Generators: generators, GoVersion: "1.26.0", ModuleName: "example.com/test"})
	if err == nil || plan != nil || !strings.Contains(err.Error(), "ambiguous dependency") {
		t.Fatalf("invalid wiring produced a plan: %v", err)
	}
	data, err := os.ReadFile(output)
	if err != nil || string(data) != sentinel {
		t.Fatal("invalid wiring changed existing output")
	}
	if _, err := os.Stat(filepath.Join(project, ".stego", "state.yaml")); !os.IsNotExist(err) {
		t.Fatal("invalid wiring wrote compiler state")
	}
}
