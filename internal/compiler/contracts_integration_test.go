package compiler_test

import (
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/compiler"
	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/grpcapplication"
	"github.com/jsell-rh/stego/internal/generator/healthcheck"
	"github.com/jsell-rh/stego/internal/generator/httpapplication"
	"github.com/jsell-rh/stego/internal/generator/jwtauth"
	"github.com/jsell-rh/stego/internal/generator/oteltracing"
	"github.com/jsell-rh/stego/internal/generator/outbox"
	"github.com/jsell-rh/stego/internal/generator/postgresadapter"
	"github.com/jsell-rh/stego/internal/generator/restapi"
	"github.com/jsell-rh/stego/internal/generator/tslsearch"
	"github.com/jsell-rh/stego/internal/slot"
	"github.com/jsell-rh/stego/internal/types"
)

//go:embed testdata/shared_storage_test.go
var sharedStorageTests []byte

const domainRuleSource = `package rules
import (
 "context"
 storage "example.com/shared/out/contracts/storage"
)
// Create records the resource and its explicit notification in one transaction.
func Create(ctx context.Context, store storage.Transactor, value any, event storage.Notification) error {
 return store.WithTransaction(ctx, func(ctx context.Context, tx storage.Transaction) error {
  if err := tx.Create(ctx, "Record", value); err != nil { return err }
  return tx.Notify(event)
 })
}
`

func TestSharedStorageDomainAndHTTP(t *testing.T) {
	t.Parallel()
	dsn, required := os.Getenv("STEGO_TEST_POSTGRES_DSN"), os.Getenv("STEGO_REQUIRE_POSTGRES")
	if required == "1" && dsn == "" {
		t.Fatal("PostgreSQL integration tests require STEGO_TEST_POSTGRES_DSN")
	}
	ctx := gen.Context{
		ModuleName: "example.com/shared", OutDirName: "out", StorageContract: "example.com/shared/out/contracts/storage",
		Entities: []types.Entity{
			{Name: "Record", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString, Unique: true}, {Name: "serial", Type: types.FieldTypeInt64}}},
			{Name: "Lease", Fields: []types.Field{{Name: "tenant", Type: types.FieldTypeString, UniqueWhenLive: true, UniqueComposite: []string{"tenant", "name"}}, {Name: "name", Type: types.FieldTypeString, UniqueWhenLive: true, UniqueComposite: []string{"tenant", "name"}}}},
			{Name: "Reservation", Fields: []types.Field{{Name: "tenant", Type: types.FieldTypeString, UniqueWhenLive: true, UniqueComposite: []string{"tenant", "name"}}, {Name: "name", Type: types.FieldTypeString, UniqueWhenLive: true, UniqueComposite: []string{"tenant", "name"}}}},
			{Name: "Alias", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString, Optional: true, Unique: true, UniqueWhenLive: true}}},
			{Name: "Membership", Fields: []types.Field{{Name: "record_id", Type: types.FieldTypeRef, To: "Record"}, {Name: "subject", Type: types.FieldTypeString}}},
		},
		Collections:    []types.Collection{{Name: "records", Entity: "Record", Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpList}}},
		PeerNamespaces: map[string]string{"rest-api": "internal/api", "postgres-adapter": "internal/store", "outbox": "internal/queue", "tsl-search": "internal/search"},
	}
	var files []gen.File
	var wirings []compiler.ComponentWiring
	for _, component := range []struct {
		name      string
		generator gen.Generator
	}{
		{"rest-api", new(restapi.Generator)}, {"postgres-adapter", new(postgresadapter.Generator)}, {"outbox", new(outbox.Generator)}, {"tsl-search", new(tslsearch.Generator)},
	} {
		ctx.OutputNamespace = ctx.PeerNamespaces[component.name]
		generated, wiring, err := component.generator.Generate(ctx)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, generated...)
		wirings = append(wirings, compiler.ComponentWiring{Name: component.name, Wiring: wiring})
	}
	shared, err := compiler.Assemble(compiler.AssemblerInput{ModuleName: ctx.ModuleName, GoVersion: "1.26.11", OutDirName: "out", Wirings: wirings})
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, shared...)
	project := t.TempDir()
	for _, file := range files {
		name := filepath.Join(project, "out", file.Path)
		if file.Path == "go.mod" {
			name = filepath.Join(project, "go.mod")
		}
		if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, file.Bytes(), 0644); err != nil {
			t.Fatal(err)
		}
	}
	for name, data := range map[string][]byte{"fills/rules/rules.go": []byte(domainRuleSource), "out/main_test.go": sharedStorageTests} {
		name = filepath.Join(project, name)
		if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"mod", "tidy"}, {"test", "-race", "-mod=readonly", "-timeout=30s", "-v", "./..."}} {
		cmd := exec.Command("go", args...)
		cmd.Dir = project
		cmd.Env = append(os.Environ(), "GOWORK=off", "STEGO_TEST_POSTGRES_DSN="+dsn, "STEGO_REQUIRE_POSTGRES="+required)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("shared storage, go %v: %v\n%s", args, err, output)
		}
		t.Log(string(output))
	}
	cmd := exec.Command("go", "list", "-mod=readonly", "-f", "{{join .Imports \"\\n\"}}", "./out/internal/store")
	cmd.Dir = project
	cmd.Env = append(os.Environ(), "GOWORK=off")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("storage imports: %v\n%s", err, output)
	}
	if strings.Contains(string(output), "/internal/api") || !strings.Contains(string(output), "/contracts/storage") {
		t.Fatalf("storage does not use the independent contract:\n%s", output)
	}

}

func TestGeneratedHealth(t *testing.T) {
	t.Parallel()
	runtimeTests := generatorTestdata(t, "healthcheck", "health_test.go")
	serviceTests := generatorTestdata(t, "healthcheck", "service_test.go")
	ctx := gen.Context{ModuleName: "example.com/probes", OutputNamespace: "health"}
	files, wiring, err := new(healthcheck.Generator).Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	authIndex := 0
	assembled, err := compiler.Assemble(compiler.AssemblerInput{ModuleName: ctx.ModuleName, GoVersion: "1.25.0", Wirings: []compiler.ComponentWiring{{Name: "health-check", Wiring: wiring}, {Name: "guard", Wiring: &gen.Wiring{
		Imports: []string{"guard"}, Constructors: []string{"guard.NewAuthMiddleware()", "guard.NewHandler()"}, MiddlewareConstructor: &authIndex, MiddlewareWrapExpr: "%s(%s)", Routes: []string{`mux.Handle("GET /private", handler)`},
	}}}})
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, assembled...)
	files = append(files, gen.File{Path: "health/health_test.go", Content: runtimeTests}, gen.File{Path: "service_test.go", Content: serviceTests}, gen.File{Path: "guard/guard.go", Content: []byte(`package guard
import "net/http"
func NewAuthMiddleware()func(http.Handler)http.Handler{return func(next http.Handler)http.Handler{return http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){if r.Header.Get("X-Test-Authorized")=="yes"{next.ServeHTTP(w,r);return};w.WriteHeader(401)})}}
func NewHandler()http.Handler{return http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){w.WriteHeader(204)})}
`)})
	project := t.TempDir()
	for _, file := range files {
		p := filepath.Join(project, file.Path)
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, file.Bytes(), 0644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"vet", "./..."}, {"test", "-race", "-count=1", "-timeout=30s", "./..."}} {
		cmd := exec.Command("go", args...)
		cmd.Dir = project
		cmd.Env = append(os.Environ(), "GOWORK=off")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("generated health: %v\n%s", err, output)
		}
	}
}

func TestGeneratedApplicationEndpoint(t *testing.T) {
	t.Parallel()
	testGeneratedApplicationEndpoint(t, "")
}

func TestGeneratedAcceptedDeletionEndpoint(t *testing.T) {
	t.Parallel()
	testGeneratedApplicationEndpoint(t, "^Test(NoContentEndpoint|DynamicReplyPreservesStatusAndBodyRules)$")
}

func TestGeneratedHTTPSPeerCertificatePin(t *testing.T) {
	t.Parallel()
	testGeneratedApplicationEndpoint(t, "^TestHTTPSPeerCertificatePin$")
}

// httpApplicationFixture is the generated HTTP application project that the tests of the HTTP application share.
var httpApplicationFixture compiler.SharedFixture

// testGeneratedApplicationEndpoint runs the tests of the generated HTTP application project, or the ones that match
// the pattern.
func testGeneratedApplicationEndpoint(t *testing.T, pattern string) {
	t.Helper()
	project := httpApplicationFixture.Dir(t, makeHTTPApplication)
	args := []string{"test", "-race", "-mod=readonly", "-count=1", "-timeout=30s", "./..."}
	if pattern != "" {
		args = append(args, "-run="+pattern)
	}
	command := exec.Command("go", args...)
	command.Dir = project
	command.Env = append(os.Environ(), "GOWORK=off")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generated HTTP application: %v\n%s", err, output)
	}
	if os.Getenv("STEGO_BENCH_PROJECTION") == "1" {
		command := exec.Command("go", "test", "-run=^$", "-bench=^BenchmarkProjectionPage$", "-benchtime=200ms", "-count=3", "./sample")
		command.Dir = project
		command.Env = append(os.Environ(), "GOWORK=off")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("projection benchmark: %v\n%s", err, output)
		}
		t.Logf("projection benchmark:\n%s", output)
	}
}

// makeHTTPApplication generates the HTTP application project into the folder given and tidies it.
func makeHTTPApplication(t *testing.T, project string) {
	t.Helper()
	ctx := gen.Context{ModuleName: "example.com/http-test", OutDirName: "out", StorageContract: "example.com/http-test/out/contracts/storage", AuthPackage: "example.com/http-test/out/auth", PeerNamespaces: map[string]string{"jwt-auth": "auth", "postgres-adapter": "store", "http-application": "application"}, Entities: []types.Entity{{Name: "Record", Fields: []types.Field{{Name: "title", Type: types.FieldTypeString}}}}}
	var files []gen.File
	var wirings []compiler.ComponentWiring
	for _, item := range []struct {
		name      string
		generator gen.Generator
		config    map[string]any
	}{
		{"postgres-adapter", new(postgresadapter.Generator), map[string]any{"migrations": "external"}},
		{"jwt-auth", new(jwtauth.Generator), map[string]any{"mode": "verifier"}},
		{"http-application", new(httpapplication.Generator), map[string]any{"factory_package": "sample"}},
	} {
		ctx.OutputNamespace = ctx.PeerNamespaces[item.name]
		ctx.ComponentConfig = item.config
		generated, wiring, err := item.generator.Generate(ctx)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, generated...)
		wirings = append(wirings, compiler.ComponentWiring{Name: item.name, Wiring: wiring})
	}
	shared, err := compiler.Assemble(compiler.AssemblerInput{ModuleName: ctx.ModuleName, OutDirName: "out", GoVersion: "1.26.11", Wirings: wirings})
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, shared...)
	for _, file := range files {
		name := filepath.Join(project, "out", file.Path)
		if file.Path == "go.mod" {
			name = filepath.Join(project, file.Path)
		}
		if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, file.Bytes(), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(project, "sample"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"sample.go", "endpoint_test.go", "lifecycle_test.go", "client_test.go", "fields_test.go"} {
		data, err := os.ReadFile(filepath.Join("..", "generator", "httpapplication", "testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(project, "sample", name), data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	completionTest, err := os.ReadFile(filepath.Join("..", "generator", "httpapplication", "testdata", "client_completion_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "out", "application", "client", "completion_test.go"), completionTest, 0644); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("go", "mod", "tidy")
	command.Dir = project
	command.Env = append(os.Environ(), "GOWORK=off")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generated HTTP application: %v\n%s", err, output)
	}
}

func TestApplicationRequiresVerifierProducer(t *testing.T) {
	ctx := gen.Context{ModuleName: "example.com/http-test", OutDirName: "out", OutputNamespace: "application", StorageContract: "example.com/http-test/out/contracts/storage", AuthPackage: "example.com/http-test/out/auth", PeerNamespaces: map[string]string{"jwt-auth": "auth"}, ComponentConfig: map[string]any{"factory_package": "sample"}}
	_, application, err := new(httpapplication.Generator).Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ctx.OutputNamespace = "auth"
	ctx.ComponentConfig = nil
	_, authentication, err := new(jwtauth.Generator).Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = compiler.Assemble(compiler.AssemblerInput{ModuleName: ctx.ModuleName, OutDirName: "out", GoVersion: "1.26.11", Wirings: []compiler.ComponentWiring{{Name: "store", Wiring: &gen.Wiring{Constructors: []string{"storage.NewStore(db)"}, Imports: []string{"storage"}, NeedsDB: true}}, {Name: "jwt-auth", Wiring: authentication}, {Name: "http-application", Wiring: application}}})
	if err == nil {
		t.Fatal("application accepted middleware in place of its verifier dependency")
	}
}

func TestGeneratedGRPCApplication(t *testing.T) {
	t.Parallel()
	for _, watch := range []bool{false, true} {
		t.Run(fmt.Sprint(watch), func(t *testing.T) {
			t.Parallel()
			testGeneratedGRPCApplication(t, watch)
		})
	}
}

func TestGeneratedStreamCompletion(t *testing.T) {
	t.Parallel()
	for _, telemetry := range []bool{false, true} {
		t.Run(fmt.Sprint(telemetry), func(t *testing.T) {
			t.Parallel()
			testGeneratedGRPCApplication(t, telemetry, "^(TestBoundedStreamTerminalObservation|TestGeneratedClientTracesCompleteCallsAndStreams)$")
		})
	}
}

func TestGeneratedAuthorityRejection(t *testing.T) {
	t.Parallel()
	testGeneratedGRPCApplication(t, false, "^TestRuntime$")
}

func TestGeneratedTLSProbe(t *testing.T) {
	t.Parallel()
	testGeneratedGRPCApplication(t, false, "^TestTLSProbe$")
}

// grpcApplicationFixtures are the generated gRPC application projects, without and with telemetry, that the tests
// of the gRPC application share.
var grpcApplicationFixtures [2]compiler.SharedFixture

// testGeneratedGRPCApplication runs the tests of the generated gRPC application project, or only the selected ones.
func testGeneratedGRPCApplication(t *testing.T, watch bool, selected ...string) {
	t.Helper()
	fixture := &grpcApplicationFixtures[0]
	if watch {
		fixture = &grpcApplicationFixtures[1]
	}
	project := fixture.Dir(t, func(t *testing.T, project string) { makeGRPCApplication(t, project, watch) })
	args := []string{"test", "-race", "-mod=readonly", "-count=1", "-timeout=30s", "./..."}
	if len(selected) != 0 {
		args = append(args[:len(args)-1], "-run", selected[0], "./...")
	}
	cmd := exec.Command("go", args...)
	cmd.Dir = project
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated gRPC application: %v\n%s", err, output)
	}
}

// makeGRPCApplication generates the gRPC application project into the folder given, and tidies and vets it.
func makeGRPCApplication(t *testing.T, project string, watch bool) {
	t.Helper()
	ctx := gen.Context{ServiceName: "records", ModuleName: "example.com/grpc-test", OutDirName: "out", EventsContract: "example.com/grpc-test/out/contracts/events", StorageContract: "example.com/grpc-test/out/contracts/storage", AuthPackage: "example.com/grpc-test/out/auth", PeerNamespaces: map[string]string{"jwt-auth": "auth", "postgres-adapter": "store", "grpc-application": "grpcapi", "outbox": "queue"}, Entities: []types.Entity{{Name: "Record", Fields: []types.Field{{Name: "title", Type: types.FieldTypeString}}}}, Inputs: map[string][]byte{"api/records.proto": []byte(grpcSchema)}}
	if watch {
		ctx.PeerNamespaces["otel-tracing"] = "telemetry"
	}
	var files []gen.File
	var wirings []compiler.ComponentWiring
	for _, item := range []struct {
		name      string
		generator gen.Generator
		config    map[string]any
	}{
		{"otel-tracing", new(oteltracing.Generator), nil},
		{"postgres-adapter", new(postgresadapter.Generator), map[string]any{"migrations": "external"}},
		{"outbox", new(outbox.Generator), nil},
		{"jwt-auth", new(jwtauth.Generator), map[string]any{"mode": "verifier"}},
		{"grpc-application", new(grpcapplication.Generator), map[string]any{"watch_events": watch, "factory_package": "sample", "proto_files": []any{map[string]any{"path": "api/records.proto", "import_path": "sample/v1/records.proto"}}}},
	} {
		if item.name == "otel-tracing" && !watch {
			continue
		}
		ctx.OutputNamespace = ctx.PeerNamespaces[item.name]
		ctx.ComponentConfig = item.config
		generated, wiring, err := item.generator.Generate(ctx)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, generated...)
		wirings = append(wirings, compiler.ComponentWiring{Name: item.name, Wiring: wiring})
	}
	shared, err := compiler.Assemble(compiler.AssemblerInput{ModuleName: ctx.ModuleName, OutDirName: "out", GoVersion: new(grpcapplication.Generator).MinimumGoVersion(), Wirings: wirings})
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, shared...)
	for _, file := range files {
		name := filepath.Join(project, "out", file.Path)
		if file.Path == "go.mod" {
			name = filepath.Join(project, file.Path)
		}
		if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, file.Bytes(), 0644); err != nil {
			t.Fatal(err)
		}
	}
	tests := []string{"bounded_stream_test.go", "sample.go", "runtime_test.go", "stream_headers_test.go", "tls_probe_test.go", "authority_test.go"}
	if watch {
		tests = append(tests, "client_telemetry_test.go")
	}
	for _, name := range tests {
		data, err := os.ReadFile(filepath.Join("..", "generator", "grpcapplication", "testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		directory := filepath.Join(project, "sample")
		if name == "bounded_stream_test.go" {
			directory = filepath.Join(project, "out/grpcapi/client")
		}
		if err := os.MkdirAll(directory, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, name), data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"mod", "tidy", "-go=" + new(grpcapplication.Generator).MinimumGoVersion()}, {"vet", "-mod=readonly", "./..."}} {
		cmd := exec.Command("go", args...)
		cmd.Dir = project
		cmd.Env = append(os.Environ(), "GOWORK=off")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("generated gRPC application: %v\n%s", err, output)
		}
	}
}

func TestGeneratedGRPCProcesses(t *testing.T) {
	t.Parallel()
	testGeneratedGRPCProcesses(t, "")
}

func TestGeneratedGRPCProcessLifecycleTelemetry(t *testing.T) {
	t.Parallel()
	testGeneratedGRPCProcesses(t, "^TestGeneratedProcess(Startup|Cleanup)Telemetry$")
}

// grpcProcessesFixture is the generated RPC process project that the tests of the RPC processes share.
var grpcProcessesFixture compiler.SharedFixture

// testGeneratedGRPCProcesses vets the generated RPC process project and runs all its tests with the race detector,
// or runs only the selected tests of its integration package.
func testGeneratedGRPCProcesses(t *testing.T, selected string) {
	t.Helper()
	project := grpcProcessesFixture.Dir(t, makeGRPCProcesses)
	commands := [][]string{{"vet", "-mod=readonly", "./..."}, {"test", "-v", "-race", "-mod=readonly", "-count=1", "-timeout=3m", "./..."}}
	if selected != "" {
		commands = [][]string{{"test", "-v", "-p=1", "-mod=readonly", "-count=1", "-timeout=45s", "-run", selected, "./integration"}}
	}
	for _, args := range commands {
		command := exec.Command("go", args...)
		command.Dir = project
		command.Env = append(os.Environ(), "GOWORK=off")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("generated RPC process: %v\n%s", err, output)
		}
		if args[0] == "test" {
			t.Log(string(output))
		}
	}
}

// makeGRPCProcesses generates the RPC process project into the folder given and tidies it.
func makeGRPCProcesses(t *testing.T, project string) {
	t.Helper()
	ctx := grpcProcessContext(t)
	ctx.StorageContract = ""
	delete(ctx.ComponentConfig, "factory_package")
	var wirings []compiler.ComponentWiring
	write := func(name string, data []byte) {
		t.Helper()
		target := filepath.Join(project, name)
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range []struct {
		namespace string
		generator gen.Generator
		config    map[string]any
	}{
		{"auth", new(jwtauth.Generator), map[string]any{"mode": "verifier"}},
		{"telemetry", new(oteltracing.Generator), nil},
		{"grpcapi", new(grpcapplication.ProcessGenerator), ctx.ComponentConfig},
	} {
		current := ctx
		current.OutputNamespace = item.namespace
		current.ComponentConfig = item.config
		files, wiring, err := item.generator.Generate(current)
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			write("out/"+file.Path, file.Bytes())
		}
		name := map[string]string{"auth": "jwt-auth", "telemetry": "otel-tracing", "grpcapi": "grpc-processes"}[item.namespace]
		wirings = append(wirings, compiler.ComponentWiring{Name: name, Wiring: wiring})
	}
	shared, err := compiler.Assemble(compiler.AssemblerInput{ModuleName: ctx.ModuleName, OutDirName: "out", GoVersion: "1.26.0", Wirings: wirings})
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range shared {
		name := "out/" + file.Path
		if file.Path == "go.mod" {
			name = file.Path
		}
		write(name, file.Bytes())
	}
	for source, target := range map[string]string{"process_factory.go": "factory/rpc.go", "process_runtime_test.go": "out/grpcapi/process/runtime_test.go", "process_health_test.go": "out/grpcapi/process/health_test.go", "process_integration_test.go": "integration/process_test.go", "process_telemetry_test.go": "integration/telemetry_test.go"} {
		data, err := os.ReadFile(filepath.Join("..", "generator", "grpcapplication", "testdata", source))
		if err != nil {
			t.Fatal(err)
		}
		write(target, data)
	}
	command := exec.Command("go", "mod", "tidy")
	command.Dir = project
	command.Env = append(os.Environ(), "GOWORK=off")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generated RPC process: %v\n%s", err, output)
	}
}

func TestGenerate_CrossGeneratorIntegrationCompilation(t *testing.T) {
	// Checklist item 95: verify that rest-api generator output, slot
	// interface/operator output, and assembler output compile together.
	// This catches signature mismatches between handler constructors and
	// the assembler's injected slot operator arguments.

	moduleName := "testmod"
	slotsPackage := "internal/slots"

	// 1. Generate rest-api files with slot bindings.
	g := &restapi.Generator{}
	ctx := gen.Context{
		Conventions: types.Convention{Layout: "flat"},
		Entities: []types.Entity{
			{Name: "User", Fields: []types.Field{
				{Name: "email", Type: types.FieldTypeString, Unique: true},
				{Name: "role", Type: types.FieldTypeEnum, Values: []string{"admin", "member"}},
			}},
		},
		Collections: []types.Collection{
			{Name: "users", Entity: "User", Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpUpdate, types.OpDelete, types.OpList}},
		},
		SlotBindings: []types.SlotDeclaration{
			{Slot: "before_create", Collection: "users", Gate: []string{"rbac-policy"}},
			{Slot: "validate", Collection: "users", Chain: []string{"validate-fill"}},
			{Slot: "on_entity_changed", Collection: "users", FanOut: []string{"audit-logger"}},
		},
		OutputNamespace: "internal/api",
		ModuleName:      moduleName,
		SlotsPackage:    slotsPackage,
	}

	apiFiles, wiring, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("rest-api Generate: %v", err)
	}

	// 2. Generate slot interface + operator files. We write the slot types
	// manually (not via GenerateInterface) to avoid duplicate type declarations
	// across files. The handler populates Input (*CreateRequest) on before-slots
	// and Entity/Action on after-slots — the types must match.
	commonTypesFile := gen.File{
		Path: slotsPackage + "/common_types.go",
		Content: []byte(`package slots

// SlotResult is generated from proto message SlotResult.
type SlotResult struct {
	Ok           bool
	ErrorMessage string
	Halt         bool
	StatusCode   int32
}

// CreateRequest is generated from proto message CreateRequest.
type CreateRequest struct {
	Entity string
	Fields map[string]string
}

// Identity is generated from proto message Identity.
type Identity struct {
	UserID string
	Role   string
}
`),
	}
	bcIfaceFile := gen.File{
		Path: slotsPackage + "/before_create.go",
		Content: []byte(`package slots

import "context"

// BeforeCreateRequest is generated from proto message BeforeCreateRequest.
type BeforeCreateRequest struct {
	Input  *CreateRequest
	Caller *Identity
}

// BeforeCreateSlot is the interface that fills implement for the BeforeCreate slot.
type BeforeCreateSlot interface {
	Evaluate(ctx context.Context, req *BeforeCreateRequest) (*SlotResult, error)
}
`),
	}
	// Operator files: use GenerateOperators for the before_create slot which
	// only needs the service definition (no imports needed for operators).
	beforeCreateProto := &slot.ProtoFile{
		Package: "stego.components.rest_api.slots",
		Services: []slot.Service{
			{Name: "BeforeCreate", Methods: []slot.Method{
				{Name: "Evaluate", InputType: "BeforeCreateRequest", OutputType: "stego.common.SlotResult"},
			}},
		},
		Messages: []slot.Message{
			{Name: "BeforeCreateRequest", Fields: []slot.MessageField{
				{Name: "input", Type: "stego.common.CreateRequest"},
				{Name: "caller", Type: "stego.common.Identity"},
			}},
		},
	}
	bcOps, err := slot.GenerateOperators(slotsPackage+"/before_create_ops.go", "slots", beforeCreateProto)
	if err != nil {
		t.Fatalf("GenerateOperators(before_create): %v", err)
	}
	oecIfaceFile := gen.File{
		Path: slotsPackage + "/on_entity_changed.go",
		Content: []byte(`package slots

import "context"

// OnEntityChangedRequest is generated from proto message OnEntityChangedRequest.
type OnEntityChangedRequest struct {
	Entity string
	Action string
}

// OnEntityChangedSlot is the interface that fills implement for the OnEntityChanged slot.
type OnEntityChangedSlot interface {
	Evaluate(ctx context.Context, req *OnEntityChangedRequest) (*SlotResult, error)
}
`),
	}
	onEntityChangedProto := &slot.ProtoFile{
		Package: "stego.mixins.event_publisher.slots",
		Services: []slot.Service{
			{Name: "OnEntityChanged", Methods: []slot.Method{
				{Name: "Evaluate", InputType: "OnEntityChangedRequest", OutputType: "stego.common.SlotResult"},
			}},
		},
		Messages: []slot.Message{
			{Name: "OnEntityChangedRequest", Fields: []slot.MessageField{
				{Name: "entity", Type: "string"},
				{Name: "action", Type: "string"},
			}},
		},
	}
	oecOps, err := slot.GenerateOperators(slotsPackage+"/on_entity_changed_ops.go", "slots", onEntityChangedProto)
	if err != nil {
		t.Fatalf("GenerateOperators(on_entity_changed): %v", err)
	}

	// Validate slot: request type has Input + entity string (no Caller).
	valIfaceFile := gen.File{
		Path: slotsPackage + "/validate.go",
		Content: []byte(`package slots

import "context"

// ValidateRequest is generated from proto message ValidateRequest.
type ValidateRequest struct {
	Input  *CreateRequest
	Entity string
}

// ValidateSlot is the interface that fills implement for the Validate slot.
type ValidateSlot interface {
	Evaluate(ctx context.Context, req *ValidateRequest) (*SlotResult, error)
}
`),
	}
	validateProto := &slot.ProtoFile{
		Package: "stego.components.rest_api.slots",
		Services: []slot.Service{
			{Name: "Validate", Methods: []slot.Method{
				{Name: "Evaluate", InputType: "ValidateRequest", OutputType: "stego.common.SlotResult"},
			}},
		},
		Messages: []slot.Message{
			{Name: "ValidateRequest", Fields: []slot.MessageField{
				{Name: "input", Type: "stego.common.CreateRequest"},
				{Name: "entity", Type: "string"},
			}},
		},
	}
	valOps, err := slot.GenerateOperators(slotsPackage+"/validate_ops.go", "slots", validateProto)
	if err != nil {
		t.Fatalf("GenerateOperators(validate): %v", err)
	}

	slotFiles := []gen.File{commonTypesFile, bcIfaceFile, bcOps, valIfaceFile, valOps, oecIfaceFile, oecOps}

	// 3. Assemble main.go from wiring + slot bindings.
	assemblerInput := compiler.AssemblerInput{
		ModuleName:  moduleName,
		ServiceName: "test-svc",
		GoVersion:   "1.22",
		Wirings: []compiler.ComponentWiring{
			{Name: "rest-api", Wiring: wiring},
			{Name: "fixture-store", Wiring: &gen.Wiring{Imports: []string{"internal/fixture"}, Constructors: []string{"fixture.NewStore()"}}},
		},
		SlotBindings: ctx.SlotBindings,
		SlotsPackage: slotsPackage,
	}
	assembledFiles, err := compiler.Assemble(assemblerInput)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	// 4. Write all files to a temp directory and compile.
	tmpDir := t.TempDir()

	goMod := "module " + moduleName + "\n\ngo 1.22\n"
	if err := os.WriteFile(filepath.Join(tmpDir, "go.mod"), []byte(goMod), 0644); err != nil {
		t.Fatalf("writing go.mod: %v", err)
	}

	// Write slot files.
	for _, f := range slotFiles {
		dst := filepath.Join(tmpDir, f.Path)
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			t.Fatalf("mkdir for %s: %v", f.Path, err)
		}
		if err := os.WriteFile(dst, f.Bytes(), 0644); err != nil {
			t.Fatalf("writing %s: %v", f.Path, err)
		}
	}

	// Write rest-api handler/router files.
	for _, f := range apiFiles {
		dst := filepath.Join(tmpDir, f.Path)
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			t.Fatalf("mkdir for %s: %v", f.Path, err)
		}
		if err := os.WriteFile(dst, f.Bytes(), 0644); err != nil {
			t.Fatalf("writing %s: %v", f.Path, err)
		}
	}

	// Write assembled files (main.go, go.mod — skip assembled go.mod since
	// we already wrote one).
	for _, f := range assembledFiles {
		if f.Path == "go.mod" {
			continue
		}
		dst := filepath.Join(tmpDir, f.Path)
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			t.Fatalf("mkdir for %s: %v", f.Path, err)
		}
		if err := os.WriteFile(dst, f.Bytes(), 0644); err != nil {
			t.Fatalf("writing %s: %v", f.Path, err)
		}
	}

	// Write stub fill packages that implement the slot interfaces.
	fillSlotTypes := map[string]struct{ iface, reqType, pkg string }{
		"rbac-policy":   {iface: "BeforeCreateSlot", reqType: "BeforeCreateRequest", pkg: "rbacpolicy"},
		"validate-fill": {iface: "ValidateSlot", reqType: "ValidateRequest", pkg: "validatefill"},
		"audit-logger":  {iface: "OnEntityChangedSlot", reqType: "OnEntityChangedRequest", pkg: "auditlogger"},
	}
	for fillName, info := range fillSlotTypes {
		fillDir := filepath.Join(tmpDir, "fills", fillName)
		if err := os.MkdirAll(fillDir, 0755); err != nil {
			t.Fatalf("mkdir for fill %s: %v", fillName, err)
		}
		stub := "package " + info.pkg + "\n\n" +
			"import (\n\t\"context\"\n\tslots \"" + moduleName + "/" + slotsPackage + "\"\n)\n\n" +
			"type impl struct{}\n\n" +
			"func New() slots." + info.iface + " { return &impl{} }\n\n" +
			"func (i *impl) Evaluate(_ context.Context, _ *slots." + info.reqType + ") (*slots.SlotResult, error) {\n" +
			"\treturn &slots.SlotResult{Ok: true}, nil\n" +
			"}\n"
		if err := os.WriteFile(filepath.Join(fillDir, "fill.go"), []byte(stub), 0644); err != nil {
			t.Fatalf("writing fill stub %s: %v", fillName, err)
		}
	}

	// Supply the test store through declared wiring. Generated code must not
	// depend on a global variable added after assembly.
	storeStub := filepath.Join(tmpDir, "internal", "fixture", "store.go")
	if err := os.MkdirAll(filepath.Dir(storeStub), 0755); err != nil {
		t.Fatal(err)
	}
	storeStubContent := "package fixture\n\nimport api \"" + moduleName + "/internal/api\"\n\nfunc NewStore() api.Storage { return nil }\n"
	if err := os.WriteFile(storeStub, []byte(storeStubContent), 0644); err != nil {
		t.Fatalf("writing store stub: %v", err)
	}

	// Compile the entire module.
	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = tmpDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cross-generator integration compilation failed:\n%s\n%s", err, output)
	}
}

// The tests below assemble the output of Supplying's generators with compiler.Assemble (ST-GEN-DS03). They sit in
// Generating's package, which may import the generators, and read the generators' test data in place.

// generatorTestdata reads a file from the test data folder of a generator package.
func generatorTestdata(t *testing.T, generator, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "generator", generator, "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

const grpcSchema = `syntax="proto3";package sample.v1;
message Request{string text=1;optional string label=2;}
message Response{string text=1;}
service Records{rpc Echo(Request)returns(Response);rpc Watch(Request)returns(stream Response);}`

func grpcProcessContext(t *testing.T) gen.Context {
	t.Helper()
	source, err := os.ReadFile(filepath.Join("..", "generator", "grpcapplication", "testdata", "process_factory.go"))
	if err != nil {
		t.Fatal(err)
	}
	return gen.Context{ServiceName: "records", ModuleName: "example.com/grpc-test", OutDirName: "out", OutputNamespace: "grpcapi", StorageContract: "example.com/grpc-test/out/contracts/storage", AuthPackage: "example.com/grpc-test/out/auth", PeerNamespaces: map[string]string{"jwt-auth": "auth", "otel-tracing": "telemetry"}, Inputs: map[string][]byte{"api.proto": []byte(grpcSchema), "factory/rpc.go": source}, ComponentConfig: map[string]any{"factory_package": "sample", "proto_files": []any{map[string]any{"path": "api.proto", "import_path": "sample/v1/records.proto"}}, "processes": []any{map[string]any{"name": "records", "factory_package": "factory"}}}}
}
