package query

import (
	"bytes"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/domainprobe"
	"github.com/jsell-rh/stego/internal/generator/postgresadapter"
)

// The service test of the generated query component: testdata/query_test.go runs in the module of the generated
// test service, with the fills of testdata/fills, against a local PostgreSQL server and a local SpiceDB container.

//go:embed testdata/query_test.go
var serviceTests []byte

//go:embed testdata/schema.zed
var serviceSchema []byte

// spiceDBImage is the pinned SpiceDB image of the authorization runtime's tests.
const spiceDBImage = "authzed/spicedb:v1.57.0@sha256:a170d372c9f6b921bb6849064df6ca3566dd5b174882c1a2c4300c2ae1d60efd"

// pinnedGo is the Go command that builds the generated service: STEGO_TEST_PINNED_GO, else bin/go of the SDK that
// runs the test.
func pinnedGo() string {
	if name := os.Getenv("STEGO_TEST_PINNED_GO"); name != "" {
		return name
	}
	return filepath.Join(runtime.GOROOT(), "bin", "go")
}

// goCommand runs the pinned Go command in the project and gives its output.
func goCommand(t *testing.T, project string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command(pinnedGo(), args...)
	cmd.Dir = project
	cmd.Env = append(append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod", "GOTOOLCHAIN=local"), env...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go %v: %v\n%s", args, err, output)
	}
	return string(output)
}

// shopProject generates the test service of testdata/service.yaml into a new module: the query component, the
// domain-probe component's probes of the four queries and the tables of the PostgreSQL adapter on the runtime data
// path, with the fills of testdata/fills and the service test. The module requires the runtime library from this
// repository's registry/runtime folder. It builds the module with the pinned Go and gives its folder.
func shopProject(t *testing.T) string {
	t.Helper()
	ctx := shopContext(t)
	files := generated(t, ctx)
	models, err := new(Generator).GoModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	probeCtx := ctx
	probeCtx.OutputNamespace = "probes"
	probeCtx.GoModelSources = map[string]gen.GoModelSource{"query": {ImportPath: shopModule + "/out/queries", Models: models}}
	probeFiles, _, err := new(domainprobe.Generator).Generate(probeCtx)
	if err != nil {
		t.Fatal(err)
	}
	adapterCtx := gen.Context{OutputNamespace: "internal/storage", ModuleName: shopModule, OutDirName: "out", ServiceName: "shop", Tenancy: ctx.Tenancy, Entities: ctx.Entities,
		PeerNamespaces:  map[string]string{"postgres-adapter": "internal/storage", gen.TenancyComponent: "internal/tenancy", "command": "internal/commands", "query": "internal/queries"},
		ComponentConfig: map[string]any{"schema_generation": "shop-v1", "writer_lease": "off"}}
	adapterFiles, _, err := new(postgresadapter.Generator).Generate(adapterCtx)
	if err != nil {
		t.Fatal(err)
	}
	var tables []byte
	for _, f := range adapterFiles {
		if f.Path == "internal/storage/migrations/001_initial.sql" {
			tables = f.Content
		}
	}
	if len(tables) == 0 {
		t.Fatal("the PostgreSQL adapter emitted no 001_initial.sql")
	}
	for _, f := range probeFiles {
		files[f.Path] = f.Bytes()
	}
	project := t.TempDir()
	write := func(name string, data []byte) {
		t.Helper()
		full := filepath.Join(project, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for name, data := range files {
		write(filepath.Join("out", name), data)
	}
	fills, err := filepath.Glob(filepath.Join("testdata", "fills", "*", "*"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range fills {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		write(strings.TrimPrefix(name, "testdata"+string(filepath.Separator)), data)
	}
	write("e2e/query_test.go", serviceTests)
	write("e2e/schema.zed", serviceSchema)
	write("e2e/tables.sql", tables)
	runtimeFolder, err := filepath.Abs(filepath.Join("..", "..", "..", "registry", "runtime"))
	if err != nil {
		t.Fatal(err)
	}
	sums, err := os.ReadFile(filepath.Join(runtimeFolder, "go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	write("go.sum", sums)
	write("go.mod", []byte("module "+shopModule+"\n\ngo "+new(Generator).MinimumGoVersion()+
		"\n\nrequire github.com/jsell-rh/stego/registry/runtime v0.38.0\n\nreplace github.com/jsell-rh/stego/registry/runtime => "+runtimeFolder+"\n"))
	goCommand(t, project, nil, "mod", "tidy")
	goCommand(t, project, nil, "vet", "./...")
	goCommand(t, project, nil, "build", "./...")
	return project
}

// startSpiceDB starts the pinned SpiceDB image in serve-testing mode, without privilege, and gives its gRPC endpoint
// and a preshared key. The container is removed when the test ends.
func startSpiceDB(t *testing.T) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatal("docker is not on the PATH: the service test needs a local SpiceDB container")
	}
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	name := "stego-query-spicedb-" + hex.EncodeToString(suffix)
	run := exec.Command("docker", "run", "-d", "--rm", "--name", name, "--memory", "256m", "--cpus", "1",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges", "-p", "127.0.0.1::50051", spiceDBImage, "serve-testing")
	if out, err := run.CombinedOutput(); err != nil {
		t.Fatalf("start the SpiceDB container: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	out, err := exec.Command("docker", "port", name, "50051/tcp").Output()
	if err != nil {
		t.Fatalf("read the port of the SpiceDB container: %v", err)
	}
	return strings.TrimSpace(strings.Split(string(out), "\n")[0]), "query-" + hex.EncodeToString(suffix)
}

// testEvent is one line of go test -json.
type testEvent struct {
	Action string
	Test   string
	Output string
}

func TestService(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a generated service")
	}
	project := shopProject(t)

	t.Run("ST-SUP-RC33-OC01-T01 The fill gets the request and one query handle", func(t *testing.T) {
		checkSlotTypes(t, project)
	})

	dsn := os.Getenv("STEGO_TEST_POSTGRES_DSN")
	if dsn == "" {
		if os.Getenv("STEGO_REQUIRE_POSTGRES") == "1" {
			t.Fatal("the service test requires STEGO_TEST_POSTGRES_DSN")
		}
		t.Skip("STEGO_TEST_POSTGRES_DSN is not set")
	}
	endpoint, key := startSpiceDB(t)
	cmd := exec.Command(pinnedGo(), "test", "-json", "-count=1", "-timeout=300s", "-run", "^TestService$", "./e2e")
	cmd.Dir = project
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod", "GOTOOLCHAIN=local",
		"STEGO_TEST_SPICEDB_ENDPOINT="+endpoint, "STEGO_TEST_SPICEDB_KEY="+key)
	output, _ := cmd.CombinedOutput()
	passed, log := map[string]bool{}, map[string]*bytes.Buffer{}
	decoder := json.NewDecoder(bytes.NewReader(output))
	for {
		var e testEvent
		if err := decoder.Decode(&e); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("read the output of the service test: %v\n%s", err, output)
		}
		if log[e.Test] == nil {
			log[e.Test] = &bytes.Buffer{}
		}
		log[e.Test].WriteString(e.Output)
		if e.Action == "pass" {
			passed[e.Test] = true
		}
	}
	for _, id := range []string{
		"ST-SUP-RC33-OC01-T02", "ST-SUP-RC33-OC05-T03", "ST-SUP-RC33-OC05-T04", "ST-SUP-RC33-OC04-T01", "ST-SUP-RC33-OC04-T02", "ST-SUP-RP11-LC17-T01", "ST-SUP-RC33-OC07-T01", "ST-SUP-RC33-OC05-T02", "ST-SUP-RC33-OC05-T01",
		"ST-SUP-RP11-LC15-T01", "ST-SUP-RC33-OC06-T01", "ST-SUP-RC33-OC06-T02", "ST-SUP-RP11-LC13-T01",
	} {
		t.Run(id+" passes in the generated service", func(t *testing.T) {
			for name, ok := range passed {
				if ok && strings.HasPrefix(name, "TestService/"+id+"_") {
					return
				}
			}
			var all strings.Builder
			for name, b := range log {
				if strings.Contains(name, id) || name == "" || name == "TestService" {
					all.WriteString(b.String())
				}
			}
			t.Fatalf("the subtest %s of the generated service did not run and pass:\n%s", id, all.String())
		})
	}
}

// checkSlotTypes type-checks the emitted contract package with the export data of its imports, and checks the fill
// type of get-order: a function of a context, the decoded request and *query.Handle only, with no parameter from
// database/sql, net or an authorization provider client (ST-SUP-RC33-OC01).
func checkSlotTypes(t *testing.T, project string) {
	t.Helper()
	out := goCommand(t, project, nil, "list", "-export", "-deps", "-json=ImportPath,Export", "./out/queries")
	exports := map[string]string{}
	decoder := json.NewDecoder(strings.NewReader(out))
	for decoder.More() {
		var p struct{ ImportPath, Export string }
		if err := decoder.Decode(&p); err != nil {
			t.Fatal(err)
		}
		exports[p.ImportPath] = p.Export
	}
	fset := token.NewFileSet()
	source, err := os.ReadFile(filepath.Join(project, "out", "queries", "queries.go"))
	if err != nil {
		t.Fatal(err)
	}
	file, err := parser.ParseFile(fset, "queries.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	lookup := func(path string) (io.ReadCloser, error) { return os.Open(exports[path]) }
	config := types.Config{Importer: importer.ForCompiler(fset, "gc", lookup)}
	pkg, err := config.Check(shopModule+"/out/queries", fset, []*ast.File{file}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if order := pkg.Scope().Lookup("Order"); order == nil {
		t.Fatal("the contract declares no Order")
	} else {
		fields := order.Type().Underlying().(*types.Struct)
		for i := 0; i < fields.NumFields(); i++ {
			if fields.Field(i).Name() == "ApiToken" {
				t.Fatal("the read-only value Order holds the secret field api_token")
			}
		}
	}
	for _, name := range []string{"GetOrderFill", "GetOrderTotalFill", "ListOrdersFill", "ListChargesFill"} {
		object := pkg.Scope().Lookup(name)
		if object == nil {
			t.Fatalf("the contract declares no %s", name)
		}
		signature, ok := object.Type().Underlying().(*types.Signature)
		if !ok {
			t.Fatalf("%s is %s, want a function type", name, object.Type().Underlying())
		}
		params := signature.Params()
		if params.Len() != 3 {
			t.Fatalf("%s has %d parameters, want the context, the request and the handle", name, params.Len())
		}
		request := strings.TrimSuffix(name, "Fill") + "Request"
		if got := params.At(0).Type().String(); got != "context.Context" {
			t.Errorf("%s takes %s first, want context.Context", name, got)
		}
		if got := params.At(1).Type().String(); got != shopModule+"/out/queries."+request {
			t.Errorf("%s takes %s second, want the decoded request %s", name, got, request)
		}
		handle, ok := params.At(2).Type().(*types.Pointer)
		if !ok || !strings.HasPrefix(handle.Elem().String(), "github.com/jsell-rh/stego/registry/runtime/query.Handle[") {
			t.Errorf("%s takes %s third, want *query.Handle", name, params.At(2).Type())
		}
		for i := 0; i < params.Len(); i++ {
			text := params.At(i).Type().String()
			for _, refused := range []string{"database/sql", "net.", "net/", "authzed", "grpc"} {
				if strings.Contains(text, refused) {
					t.Errorf("%s takes %s, a type of %s", name, text, refused)
				}
			}
		}
		// The request reaches nothing either: its fields are plain values and the cursor.
		structure := params.At(1).Type().Underlying().(*types.Struct)
		for i := 0; i < structure.NumFields(); i++ {
			field := structure.Field(i).Type().String()
			if field != "string" && field != "int" && field != shopModule+"/out/queries.Cursor" {
				t.Errorf("the request %s has the field %s of type %s", request, structure.Field(i).Name(), field)
			}
		}
	}
}
