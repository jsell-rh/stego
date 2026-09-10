package compiler

import (
	_ "embed"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
)

//go:embed testdata/task_lifecycle_test.go
var taskLifecycleTests []byte

const taskComponentSource = `package tasks
import ("context"; "errors"; "sync/atomic"; "os")
var ErrTask = errors.New("task failure")
var Closed atomic.Bool
var Stopped atomic.Bool
var Started chan struct{}
type Resource struct{}
func NewResource() *Resource { Closed.Store(false); Stopped.Store(false); Started = make(chan struct{}); return &Resource{} }
func (*Resource) Close() { if !Stopped.Load() { panic("cleanup before task stop") }; Closed.Store(true) }
type Worker struct{}
func NewWorker(*Resource) *Worker { return &Worker{} }
func (*Worker) Run(ctx context.Context) error {
 <-Started
 if os.Getenv("TASK_SIGNAL") == "1" { if err := os.WriteFile(os.Getenv("TASK_READY"), []byte("ready"), 0600); err != nil { return err }; <-ctx.Done(); return ctx.Err() }
 return ErrTask
}
type Context struct{}
func NewContext(*Resource) *Context { return &Context{} }
func (*Context) Run(ctx context.Context) error {
 close(Started)
 <-ctx.Done()
 if Closed.Load() { panic("resource closed during task use") }
 Stopped.Store(true)
 return ctx.Err()
}
`

func TestGeneratedBackgroundLifecycle(t *testing.T) {
	for _, withHTTP := range []bool{false, true} {
		name := "worker-only"
		if withHTTP {
			name = "http-and-workers"
		}
		t.Run(name, func(t *testing.T) {
			wiring := &gen.Wiring{
				Imports:               []string{"internal/tasks"},
				Constructors:          []string{"tasks.NewResource()", "tasks.NewWorker(resource)", "tasks.NewContext(resource)"},
				ConstructorDeps:       map[int][]string{1: {"resource"}, 2: {"resource"}},
				ConstructorDeferCalls: map[int]string{0: "Close()"},
				BackgroundTasks:       []int{1, 2},
			}
			if withHTTP {
				wiring.Routes = []string{`mux.Handle("/", http.NotFoundHandler())`}
			}
			files, err := Assemble(AssemblerInput{ModuleName: "example.com/tasks", GoVersion: "1.26.8", Wirings: []ComponentWiring{
				{Name: "tasks", Wiring: wiring},
				{Name: "duplicate-name", Wiring: &gen.Wiring{Imports: []string{"internal/tasks"}, Constructors: []string{"tasks.NewWorker(resource)"}, ConstructorDeps: map[int][]string{0: {"resource"}}, BackgroundTasks: []int{0}}},
			}})
			if err != nil {
				t.Fatal(err)
			}
			project := t.TempDir()
			for _, file := range files {
				if err := os.WriteFile(filepath.Join(project, file.Path), file.Bytes(), 0644); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.MkdirAll(filepath.Join(project, "internal/tasks"), 0755); err != nil {
				t.Fatal(err)
			}
			for name, content := range map[string][]byte{"internal/tasks/tasks.go": []byte(taskComponentSource), "main_test.go": taskLifecycleTests} {
				if err := os.WriteFile(filepath.Join(project, name), content, 0644); err != nil {
					t.Fatal(err)
				}
			}
			if withHTTP {
				if err := os.WriteFile(filepath.Join(project, "http_test.go"), taskHTTPTests, 0644); err != nil {
					t.Fatal(err)
				}
			}
			command := exec.Command("go", "test", "-race", "-mod=readonly", "-timeout=20s", "./...")
			command.Dir = project
			command.Env = append(os.Environ(), "GOWORK=off")
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("generated lifecycle: %v\n%s", err, output)
			}
			if os.Getenv("STEGO_CROSS_COMPILE") == "1" {
				for _, target := range []struct{ os, arch string }{{"windows", "amd64"}, {"darwin", "arm64"}} {
					command := exec.Command("go", "test", "-c", "-mod=readonly", "-o", filepath.Join(t.TempDir(), "task-test"))
					command.Dir = project
					command.Env = append(os.Environ(), "GOWORK=off", "CGO_ENABLED=0", "GOOS="+target.os, "GOARCH="+target.arch)
					if output, err := command.CombinedOutput(); err != nil {
						t.Fatalf("cross compilation %s/%s: %v\n%s", target.os, target.arch, err, output)
					}
				}
			}
		})
	}
}

func TestBackgroundTaskIndexes(t *testing.T) {
	for _, indexes := range [][]int{{-1}, {1}, {0, 0}} {
		_, err := Assemble(AssemblerInput{ModuleName: "example.com/tasks", GoVersion: "1.26.8", Wirings: []ComponentWiring{{Name: "tasks", Wiring: &gen.Wiring{Constructors: []string{"tasks.NewWorker()"}, BackgroundTasks: indexes}}}})
		if err == nil || !strings.Contains(err.Error(), "background task index") {
			t.Fatalf("invalid indexes %v did not reach task validation: %v", indexes, err)
		}
	}
}

//go:embed testdata/task_http_test.go
var taskHTTPTests []byte

func TestBackgroundTaskConsumesDatabaseDependency(t *testing.T) {
	input := AssemblerInput{Wirings: []ComponentWiring{
		{Name: "store", Wiring: &gen.Wiring{Constructors: []string{"storage.NewStore(db)"}, NeedsDB: true}},
		{Name: "worker", Wiring: &gen.Wiring{Constructors: []string{"worker.NewWorker(store)"}, ConstructorDeps: map[int][]string{0: {"store"}}, BackgroundTasks: []int{0}}},
		{Name: "unused", Wiring: &gen.Wiring{Constructors: []string{"auth.NewAuth()"}, MiddlewareConstructor: new(int), MiddlewareWrapExpr: "%s(%s)"}},
	}}
	consumed, hasDB := computeConsumedConstructors(input, false)
	if !hasDB || len(consumed) != 2 || !consumed[constructorKey{0, 0}] || !consumed[constructorKey{1, 0}] {
		t.Fatalf("worker dependencies were not preserved: db=%v, constructors=%v", hasDB, consumed)
	}
}
