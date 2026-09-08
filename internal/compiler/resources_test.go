package compiler

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
)

func TestResourceArgumentsUseTheSelectedDatabase(t *testing.T) {
	dsn := os.Getenv("STEGO_TEST_POSTGRES_DSN")
	if dsn == "" {
		if os.Getenv("STEGO_REQUIRE_POSTGRES") == "1" {
			t.Fatal("PostgreSQL is required")
		}
		t.Skip("set STEGO_TEST_POSTGRES_DSN to test runtime resources")
	}
	for _, backend := range []string{"sql", "gorm"} {
		t.Run(backend, func(t *testing.T) {
			wiring := &gen.Wiring{Imports: []string{"probe"}, Constructors: []string{"probe.NewProbe()"}, ConstructorResources: map[int][]gen.Resource{0: {gen.ServiceContext, gen.SQLDatabase}}, ConstructorReturnsError: map[int]bool{0: true}, BackgroundTasks: []int{0}, DBBackend: backend}
			if backend == "gorm" {
				wiring.GoModRequires = map[string]string{"gorm.io/gorm": "v1.25.12", "gorm.io/driver/postgres": "v1.5.11"}
			}
			files, err := Assemble(AssemblerInput{ModuleName: "example.com/resources", GoVersion: "1.26.8", Wirings: []ComponentWiring{{Name: "probe", Wiring: wiring}}})
			if err != nil {
				t.Fatal(err)
			}
			project := t.TempDir()
			for _, file := range files {
				if err := os.WriteFile(filepath.Join(project, file.Path), file.Bytes(), 0644); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Mkdir(filepath.Join(project, "probe"), 0755); err != nil {
				t.Fatal(err)
			}
			source := `package probe
import("context";"database/sql";"errors";"time")
var ErrDone=errors.New("probe complete")
type Probe struct{}
func NewProbe(ctx context.Context, db *sql.DB)(*Probe,error){
 if ctx==nil||db==nil{return nil,errors.New("resource is absent")}
 ctx,cancel:=context.WithTimeout(ctx,3*time.Second);defer cancel()
 var value int
 if err:=db.QueryRowContext(ctx,"SELECT 1").Scan(&value);err!=nil{return nil,err}
 if value!=1{return nil,errors.New("unexpected query result")}
 return &Probe{},nil
}
func(*Probe)Run(context.Context)error{return ErrDone}
`
			test := `package main
import("errors";"testing";"example.com/resources/probe")
func TestResources(t *testing.T){if err:=run();!errors.Is(err,probe.ErrDone){t.Fatalf("runtime resources: %v",err)}}
`
			for name, content := range map[string]string{"probe/probe.go": source, "main_test.go": test} {
				if err := os.WriteFile(filepath.Join(project, name), []byte(content), 0644); err != nil {
					t.Fatal(err)
				}
			}
			for _, args := range [][]string{{"mod", "tidy"}, {"test", "-race", "-mod=readonly", "-timeout=20s", "./..."}} {
				command := exec.Command("go", args...)
				command.Dir = project
				command.Env = append(os.Environ(), "GOWORK=off", "DATABASE_URL="+dsn)
				if output, err := command.CombinedOutput(); err != nil {
					t.Fatalf("generated %s resource test: %v\n%s", backend, err, output)
				}
			}
		})
	}
}

func TestInvalidConstructorResourcesFailAssembly(t *testing.T) {
	for _, resources := range []map[int][]gen.Resource{{-1: {gen.ServiceContext}}, {1: {gen.SQLDatabase}}, {0: {"unknown"}}} {
		_, err := Assemble(AssemblerInput{ModuleName: "example.com/resources", GoVersion: "1.26.8", Wirings: []ComponentWiring{{Name: "probe", Wiring: &gen.Wiring{Constructors: []string{"probe.New()"}, ConstructorResources: resources}}}})
		if err == nil {
			t.Fatalf("invalid resources were accepted: %v", resources)
		}
	}
}

func TestConstructorDependenciesMustExist(t *testing.T) {
	for _, dependency := range []string{"missing", "worker", "db", "ctx"} {
		_, err := Assemble(AssemblerInput{ModuleName: "example.com/resources", GoVersion: "1.26.8", Wirings: []ComponentWiring{{Name: "tasks", Wiring: &gen.Wiring{Imports: []string{"tasks"}, Constructors: []string{"tasks.NewWorker(" + dependency + ")"}, ConstructorDeps: map[int][]string{0: {dependency}}, BackgroundTasks: []int{0}}}}})
		// Worker services have a service context, but no implicit database.
		if dependency == "ctx" {
			if err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err == nil {
			t.Fatalf("unresolved or self dependency was accepted: %s", dependency)
		}
	}
}
