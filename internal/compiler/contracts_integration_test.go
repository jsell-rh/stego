package compiler_test

import (
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/jsell-rh/stego/internal/compiler"
	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/grpcapplication"
	"github.com/jsell-rh/stego/internal/generator/healthcheck"
	"github.com/jsell-rh/stego/internal/generator/httpapplication"
	"github.com/jsell-rh/stego/internal/generator/jwtauth"
	"github.com/jsell-rh/stego/internal/generator/oteltracing"
)

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
	// The application factory gets the verifier alone and no storage (ST-SUP-RC10-OC31), so the project has no
	// PostgreSQL adapter.
	ctx := gen.Context{ModuleName: "example.com/http-test", OutDirName: "out", AuthPackage: "example.com/http-test/out/auth", PeerNamespaces: map[string]string{"jwt-auth": "auth", "http-application": "application"}}
	var files []gen.File
	var wirings []compiler.ComponentWiring
	for _, item := range []struct {
		name      string
		generator gen.Generator
		config    map[string]any
	}{
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
	ctx := gen.Context{ModuleName: "example.com/http-test", OutDirName: "out", OutputNamespace: "application", AuthPackage: "example.com/http-test/out/auth", PeerNamespaces: map[string]string{"jwt-auth": "auth"}, ComponentConfig: map[string]any{"factory_package": "sample"}}
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

// makeGRPCApplication generates the project of the gRPC transport, the client and the stream helpers into the folder
// given, and tidies and vets it. grpc-processes emits them with no data access; grpc-application emits the same
// transport runtime for the declared commands and queries, and its former bridge to the GORM store is gone
// (ST-SUP-RC20-OC39). watch adds the client telemetry tests.
func makeGRPCApplication(t *testing.T, project string, watch bool) {
	t.Helper()
	ctx := grpcProcessContext(t)
	delete(ctx.ComponentConfig, "factory_package")
	ctx.PeerNamespaces["grpc-processes"] = "grpcapi"
	var files []gen.File
	var wirings []compiler.ComponentWiring
	for _, item := range []struct {
		name, namespace string
		generator       gen.Generator
		config          map[string]any
	}{
		{"otel-tracing", "telemetry", new(oteltracing.Generator), nil},
		{"jwt-auth", "auth", new(jwtauth.Generator), map[string]any{"mode": "verifier"}},
		{"grpc-processes", "grpcapi", new(grpcapplication.ProcessGenerator), ctx.ComponentConfig},
	} {
		current := ctx
		current.OutputNamespace = item.namespace
		current.ComponentConfig = item.config
		generated, wiring, err := item.generator.Generate(current)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, generated...)
		wirings = append(wirings, compiler.ComponentWiring{Name: item.name, Wiring: wiring})
	}
	files = append(files, gen.File{Path: "../factory/rpc.go", Content: ctx.Inputs["factory/rpc.go"]})
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
	return gen.Context{ServiceName: "records", ModuleName: "example.com/grpc-test", OutDirName: "out", OutputNamespace: "grpcapi", AuthPackage: "example.com/grpc-test/out/auth", PeerNamespaces: map[string]string{"jwt-auth": "auth", "otel-tracing": "telemetry"}, Inputs: map[string][]byte{"api.proto": []byte(grpcSchema), "factory/rpc.go": source}, ComponentConfig: map[string]any{"factory_package": "sample", "proto_files": []any{map[string]any{"path": "api.proto", "import_path": "sample/v1/records.proto"}}, "processes": []any{map[string]any{"name": "records", "factory_package": "factory"}}}}
}
