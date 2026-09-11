package compiler

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jsell-rh/stego/internal/gen"
)

func TestDatabaseOpenerValidation(t *testing.T) {
	for _, change := range []func(*gen.Wiring){
		func(w *gen.Wiring) { w.NeedsDB = false },
		func(w *gen.Wiring) { w.DBBackend = "unknown" },
		func(w *gen.Wiring) { w.DatabaseOpener.Namespace = "../storage" },
		func(w *gen.Wiring) { w.DatabaseOpener.Namespace = "other" },
		func(w *gen.Wiring) { w.DatabaseOpener.Function = "Open();panic(1)" },
		func(w *gen.Wiring) { w.DatabaseOpener.Function = "open" },
	} {
		w := databaseOpenerFixture()
		change(w)
		if files, err := Assemble(AssemblerInput{ModuleName: "example.com/probe", Wirings: []ComponentWiring{{Name: "store", Wiring: w}}}); err == nil || len(files) != 0 {
			t.Fatal("invalid database opener produced output")
		}
	}
	if err := validateDatabaseOpener([]ComponentWiring{{Name: "one", Wiring: databaseOpenerFixture()}, {Name: "two", Wiring: databaseOpenerFixture()}}); err == nil {
		t.Fatal("ambiguous database opener accepted")
	}
}

func databaseOpenerFixture() *gen.Wiring {
	return &gen.Wiring{NeedsDB: true, Imports: []string{"storage"}, Constructors: []string{"storage.NewStore(db)"}, DatabaseOpener: &gen.DatabaseOpenerSpec{Namespace: "storage", Function: "OpenDatabase"}}
}

func TestDatabaseOpenerAssembly(t *testing.T) {
	for _, backend := range []string{"", "gorm"} {
		for _, used := range []bool{false, true} {
			w := databaseOpenerFixture()
			w.DBBackend = backend
			if used {
				w.Routes = []string{`mux.Handle("/",store)`}
			}
			files, err := Assemble(AssemblerInput{ModuleName: "example.com/probe", GoVersion: "1.26.8", Wirings: []ComponentWiring{{Name: "store", Wiring: w}}})
			if err != nil {
				t.Fatal(err)
			}
			for _, f := range files {
				if f.Path == "main.go" {
					s := string(f.Bytes())
					if strings.Contains(s, "storage.OpenDatabase(dsn)") != used {
						t.Fatal("database opener consumption differs from its resource")
					}
					if used && backend == "gorm" && (!strings.Contains(s, "postgres.Config{Conn: sqlDB}") || !strings.Contains(s, "defer sqlDB.Close()")) {
						t.Fatal("GORM did not use the owned pool")
					}
					if used && backend == "" && strings.Contains(s, `"database/sql"`) {
						t.Fatal("custom SQL opener added an unused SQL import")
					}
				}
			}
		}
	}
}

func TestDatabaseOpenerUsesRenamedImport(t *testing.T) {
	input := AssemblerInput{Wirings: []ComponentWiring{{Name: "store", Wiring: databaseOpenerFixture()}}}
	imports := importResult{Renames: map[int]map[string]string{0: {"storage": "storage2"}}}
	if databaseOpenExpression(input, imports, map[int]bool{0: true}) != "storage2.OpenDatabase(dsn)" {
		t.Fatal("opener ignored its import alias")
	}
}

func TestDatabasePoolClosesOnStartupFailure(t *testing.T) {
	for _, backend := range []string{"sql", "gorm"} {
		t.Run(backend, func(t *testing.T) { testDatabaseStartupFailure(t, backend) })
	}
}

func testDatabaseStartupFailure(t *testing.T, backend string) {
	w := databaseOpenerFixture()
	if backend == "gorm" {
		w.DBBackend = "gorm"
	}
	w.Imports = []string{"internal/postgres"}
	w.Constructors = []string{"postgres.NewStore(db)"}
	w.DatabaseOpener.Namespace = "internal/postgres"
	w.Routes = []string{`mux.Handle("/",store)`}
	w.GoModRequires = map[string]string{"gorm.io/gorm": "v1.25.12", "gorm.io/driver/postgres": "v1.5.11", "github.com/jackc/pgx/v5": "v5.11.0"}
	files, err := Assemble(AssemblerInput{ModuleName: "example.com/pool", GoVersion: "1.26.8", Wirings: []ComponentWiring{{Name: "store", Wiring: w}}})
	if err != nil {
		t.Fatal(err)
	}
	storeSource := `package postgres
import("context";"database/sql";"database/sql/driver";"errors";"net/http";"os";"time";"gorm.io/gorm")
type Store struct{}
func NewStore(*gorm.DB)*Store{return &Store{}}
func(*Store)ServeHTTP(http.ResponseWriter,*http.Request){}
func OpenDatabase(string)(*sql.DB,error){return sql.OpenDB(connector{}),nil}
type connector struct{}
func(connector)Connect(context.Context)(driver.Conn,error){return connection{},nil}
func(connector)Driver()driver.Driver{return drv{}}
type drv struct{}
func(drv)Open(string)(driver.Conn,error){return connection{},nil}
type connection struct{}
func(connection)Prepare(string)(driver.Stmt,error){return nil,errors.New("unused")}
func(connection)Begin()(driver.Tx,error){return nil,errors.New("unused")}
func(connection)Ping(ctx context.Context)error{
 deadline,ok:=ctx.Deadline()
 if ok&&time.Until(deadline)>0&&time.Until(deadline)<=5*time.Second{os.WriteFile(os.Getenv("POOL_PING_DEADLINE"),[]byte("bounded"),0600)}
 return errors.New("private-ping-failure")
}
func(connection)Close()error{return os.WriteFile(os.Getenv("POOL_CLOSED"),[]byte("closed"),0600)}
`
	if backend == "sql" {
		storeSource = strings.ReplaceAll(storeSource, `;"gorm.io/gorm"`, "")
		storeSource = strings.ReplaceAll(storeSource, "*gorm.DB", "*sql.DB")
	}
	files = append(files, gen.File{Path: "internal/postgres/store.go", Content: []byte(storeSource)})
	project := t.TempDir()
	for _, file := range files {
		name := filepath.Join(project, file.Path)
		if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, file.Bytes(), 0644); err != nil {
			t.Fatal(err)
		}
	}
	binary := filepath.Join(project, "service")
	for _, args := range [][]string{{"mod", "tidy"}, {"build", "-race", "-o", binary, "."}} {
		command := exec.Command("go", args...)
		command.Dir = project
		command.Env = append(os.Environ(), "GOWORK=off")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("pool assembly: %v %s", err, output)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	marker := filepath.Join(project, "closed")
	deadlineMarker := filepath.Join(project, "ping-deadline")
	command := exec.CommandContext(ctx, binary)
	command.Env = append(os.Environ(), "DATABASE_URL=private-database-url", "POOL_CLOSED="+marker, "POOL_PING_DEADLINE="+deadlineMarker, "GORACE=atexit_sleep_ms=0")
	output, err := command.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 || ctx.Err() != nil {
		t.Fatal("database failure did not stop the process", err)
	}
	if strings.Contains(string(output), "private-") || !strings.Contains(string(output), "database.ping") {
		t.Fatal("database failure record is unsafe or missing")
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "closed" {
		t.Fatal("database pool was not closed", err)
	}
	if data, err := os.ReadFile(deadlineMarker); err != nil || string(data) != "bounded" {
		t.Fatal("database startup ping had no deadline", err)
	}
}
