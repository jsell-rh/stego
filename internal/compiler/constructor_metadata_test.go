package compiler

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
)

func metadataInput(routes bool) AssemblerInput {
	wiring := &gen.Wiring{Imports: []string{"internal/fixture"}, Constructors: []string{"fixture.NewHandler()"}}
	if routes {
		wiring.Routes = []string{`mux.HandleFunc("GET /private", handler.Handle)`}
	}
	return AssemblerInput{ModuleName: "example.com/metadata", GoVersion: "1.26.8", OutDirName: "out", Wirings: []ComponentWiring{{Name: "fixture", Wiring: wiring}}}
}

func TestConstructorMetadataIndexesMustExist(t *testing.T) {
	fields := []struct {
		name string
		set  func(*gen.Wiring, int)
	}{
		{"resources", func(w *gen.Wiring, i int) { w.ConstructorResources = map[int][]gen.Resource{i: {gen.ServiceContext}} }},
		{"tasks", func(w *gen.Wiring, i int) { w.BackgroundTasks = []int{i} }},
		{"error result", func(w *gen.Wiring, i int) { w.ConstructorReturnsError = map[int]bool{i: false} }},
		{"collections", func(w *gen.Wiring, i int) { w.ConstructorCollections = map[int]string{i: "items"} }},
		{"dependencies", func(w *gen.Wiring, i int) { w.ConstructorDeps = map[int][]string{i: {}} }},
		{"cleanup", func(w *gen.Wiring, i int) { w.ConstructorDeferCalls = map[int]string{i: "Close()"} }},
		{"HTTP error logger", func(w *gen.Wiring, i int) { w.HTTPErrorLogger = &i }},
		{"primary middleware", func(w *gen.Wiring, i int) { w.MiddlewareConstructor = &i; w.MiddlewareWrapExpr = "%s(%s)" }},
		{"inner middleware", func(w *gen.Wiring, i int) {
			w.Middlewares = []gen.MiddlewareSpec{{ConstructorIndex: i, WrapExpr: "%s(%s)"}}
		}},
		{"outer middleware", func(w *gen.Wiring, i int) {
			w.OuterMiddlewares = []gen.MiddlewareSpec{{ConstructorIndex: i, WrapExpr: "%s(%s)"}}
		}},
	}
	// A new constructor-indexed map must have an invalid-index test.
	covered := map[string]bool{}
	kind := reflect.TypeOf(gen.Wiring{})
	for _, field := range fields {
		var wiring gen.Wiring
		field.set(&wiring, 0)
		value := reflect.ValueOf(wiring)
		for i := range kind.NumField() {
			declaration := kind.Field(i)
			if strings.HasPrefix(declaration.Name, "Constructor") && declaration.Type.Kind() == reflect.Map && declaration.Type.Key().Kind() == reflect.Int && !value.Field(i).IsNil() {
				covered[declaration.Name] = true
			}
		}
	}
	for i := range kind.NumField() {
		declaration := kind.Field(i)
		if strings.HasPrefix(declaration.Name, "Constructor") && declaration.Type.Kind() == reflect.Map && declaration.Type.Key().Kind() == reflect.Int && !covered[declaration.Name] {
			t.Fatal("constructor metadata map has no index test", declaration.Name)
		}
	}
	for _, field := range fields {
		for _, routes := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/routes=%t", field.name, routes), func(t *testing.T) {
				valid := metadataInput(routes)
				field.set(valid.Wirings[0].Wiring, 0)
				if files, err := Assemble(valid); err != nil || len(files) == 0 {
					t.Fatal("valid metadata fixture failed", err)
				}
				empty := metadataInput(routes)
				empty.Wirings[0].Wiring.Constructors = nil
				field.set(empty.Wirings[0].Wiring, 0)
				if files, err := Assemble(empty); err == nil || len(files) != 0 {
					t.Error("metadata for an absent constructor returned output")
				}
				for _, index := range []int{-1, 1, 1000} {
					input := metadataInput(routes)
					field.set(input.Wirings[0].Wiring, index)
					if files, err := Assemble(input); err == nil || len(files) != 0 {
						t.Errorf("invalid %s index %d returned %d files", field.name, index, len(files))
					}
				}
			})
		}
	}
}

func TestPrimaryMiddlewareSelectionMustBeUnambiguous(t *testing.T) {
	for _, routes := range []bool{false, true} {
		input := metadataInput(routes)
		input.Wirings[0].Wiring.MiddlewareWrapExpr = "%s(%s)"
		if files, err := Assemble(input); err == nil || len(files) != 0 {
			t.Error("wrapper without a constructor was ignored")
		}
		index := 0
		input.Wirings[0].Wiring.MiddlewareConstructor = &index
		other := metadataInput(routes).Wirings[0]
		other.Name = "other"
		if routes {
			other.Wiring.Routes = []string{`mux.HandleFunc("GET /other", handler.Handle)`}
		}
		other.Wiring.MiddlewareConstructor = &index
		other.Wiring.MiddlewareWrapExpr = "%s(%s)"
		input.Wirings = append(input.Wirings, other)
		if files, err := Assemble(input); err == nil || len(files) != 0 {
			t.Error("multiple primary middleware constructors were accepted")
		}
	}
}

func TestConstructorMetadataDiagnosticsAreDeterministic(t *testing.T) {
	input := metadataInput(true)
	input.Wirings[0].Wiring.ConstructorResources = map[int][]gen.Resource{99: {gen.ServiceContext}, -7: {gen.SQLDatabase}, 4: {gen.ServiceContext}}
	input.Wirings[0].Wiring.ConstructorCollections = map[int]string{-8: "items"}
	want := ""
	for range 128 {
		files, err := Assemble(input)
		if err == nil || len(files) != 0 {
			t.Fatal("invalid metadata produced output")
		}
		if want == "" {
			want = err.Error()
		}
		if err.Error() != want || !strings.Contains(want, "resource constructor index -7") {
			t.Fatal("metadata diagnostics changed", want, err)
		}
	}
}

func BenchmarkConstructorMetadataValidation(b *testing.B) {
	wiring := &gen.Wiring{
		ConstructorResources: map[int][]gen.Resource{}, ConstructorReturnsError: map[int]bool{},
		ConstructorCollections: map[int]string{}, ConstructorDeps: map[int][]string{}, ConstructorDeferCalls: map[int]string{},
	}
	for i := range 256 {
		wiring.Constructors = append(wiring.Constructors, fmt.Sprintf("fixture.NewHandler%d()", i))
		wiring.ConstructorResources[i] = []gen.Resource{gen.ServiceContext}
		wiring.ConstructorReturnsError[i] = true
		wiring.ConstructorCollections[i] = "items"
		wiring.ConstructorDeps[i] = nil
		wiring.ConstructorDeferCalls[i] = "Close()"
		wiring.BackgroundTasks = append(wiring.BackgroundTasks, i)
		wiring.Middlewares = append(wiring.Middlewares, gen.MiddlewareSpec{ConstructorIndex: i, WrapExpr: "%s(%s)"})
	}
	input := []ComponentWiring{{Name: "metadata", Wiring: wiring}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := validateConstructorMetadata(input); err != nil {
			b.Fatal(err)
		}
	}
}
