package compiler

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
)

func TestHTTPLoggerSelectionMustBeUnambiguous(t *testing.T) {
	index := 0
	for _, routes := range []bool{false, true} {
		input := metadataInput(routes)
		input.Wirings[0].Wiring.HTTPErrorLogger = &index
		other := metadataInput(false).Wirings[0]
		other.Name = "other"
		other.Wiring.HTTPErrorLogger = &index
		input.Wirings = append(input.Wirings, other)
		if files, err := Assemble(input); err == nil || len(files) != 0 {
			t.Fatal("ambiguous HTTP logger produced files")
		}
	}
}

func TestHTTPLoggerConsumesItsConstructorAndUsesItsName(t *testing.T) {
	index := 0
	for _, routes := range []bool{false, true} {
		for _, tasks := range []bool{false, true} {
			api := &gen.Wiring{Imports: []string{"internal/api"}, Constructors: []string{"api.NewHandle()"}}
			if routes {
				api.Routes = []string{`mux.Handle("/",handle)`}
			}
			if tasks {
				api.BackgroundTasks = []int{0}
			}
			files, err := Assemble(AssemblerInput{ModuleName: "example.com/diagnostics", GoVersion: "1.26.8", Wirings: []ComponentWiring{
				{Name: "api", Wiring: api},
				{Name: "logger", Wiring: &gen.Wiring{Imports: []string{"internal/log"}, Constructors: []string{"log.NewHandle()"}, HTTPErrorLogger: &index}},
			}})
			if err != nil {
				t.Fatal(err)
			}
			project := t.TempDir()
			for _, file := range files {
				if file.Path == "main.go" && strings.Contains(string(file.Bytes()), "handle2.HTTPErrorLog()") != routes {
					t.Fatal("HTTP logger used the wrong constructor name")
				}
				if err := os.WriteFile(filepath.Join(project, file.Path), file.Bytes(), 0644); err != nil {
					t.Fatal(err)
				}
			}
			sources := map[string]string{
				"internal/api/api.go": `package api
import("context";"net/http")
type Handle struct{}
func NewHandle()*Handle{return &Handle{}}
func(*Handle)ServeHTTP(http.ResponseWriter,*http.Request){}
func(*Handle)Run(ctx context.Context)error{<-ctx.Done();return ctx.Err()}
`,
				"internal/log/log.go": `package log
import("log";"io")
type Handle struct{}
func NewHandle()*Handle{return &Handle{}}
func(*Handle)HTTPErrorLog()*log.Logger{return log.New(io.Discard,"",0)}
`,
			}
			for name, source := range sources {
				full := filepath.Join(project, name)
				if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(full, []byte(source), 0644); err != nil {
					t.Fatal(err)
				}
			}
			command := exec.Command("go", "test", "-race", "-mod=readonly", "./...")
			command.Dir = project
			command.Env = append(os.Environ(), "GOWORK=off")
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("generated logger assembly: %v\n%s", err, output)
			}
		}
	}
}
