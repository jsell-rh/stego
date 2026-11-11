package compiler

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The proof of the gRPC application on the runtime data path (ST-SUP-RC07-OC19, ST-SUP-RC33-OC08): a service of the
// rpc-service archetype with the command and query components, generated from testdata/runtime_grpc/project/service.yaml
// with stego apply, built with the pinned Go, and run against a local PostgreSQL server and a local SpiceDB container.

// grpcFixture is the folder of the gRPC test service: its project files and the check that runs in its module.
const grpcFixture = "testdata/runtime_grpc"

// rpcRuntimeArchetype is the components list of the rpc-service archetype of the registry copy on the runtime data
// path: the command and query generators with the PostgreSQL adapter, the tenancy, domain-probe and outbox components
// that the runtime wiring calls, and the gRPC application in place of grpc-processes. The shipped archetypes move to
// the runtime data path in a later step.
const rpcRuntimeArchetype = "components:\n  - postgres-adapter\n  - tenancy\n  - domain-probe\n  - command\n  - query\n  - outbox\n  - grpc-application\n  - otel-tracing\n  - health-check\n"

// newGRPCProject copies the registry of this repository with the rpc-service archetype on the runtime data path, each
// alpha component of the test staged as ga, and the project files of the fixture into a new folder.
func newGRPCProject(t *testing.T, binary string) *wiringProject {
	t.Helper()
	p := &wiringProject{t: t, dir: t.TempDir(), binary: binary}
	if err := os.CopyFS(filepath.Join(p.dir, "registry"), os.DirFS(filepath.Join("..", "..", "registry"))); err != nil {
		t.Fatal(err)
	}
	archetype := filepath.Join("registry", "archetypes", "rpc-service", "archetype.yaml")
	text := regexp.MustCompile(`(?s)components:\n(  - [a-z-]+\n)+`).ReplaceAllString(p.read(archetype), rpcRuntimeArchetype)
	// The PostgreSQL adapter gives the storage port that grpc-application requires; grpc-processes is not active.
	text = strings.Replace(text, "  application-grpc-processes: grpc-processes\n", "", 1)
	p.write(archetype, strings.Replace(text, "bindings:\n", "bindings:\n  storage-adapter: postgres-adapter\n", 1))
	sunset := regexp.MustCompile(`sunset: "?[0-9-]+"?\n`)
	for _, name := range []string{"tenancy", "command", "query", "domain-probe", "audit-log", "idempotency", "spicedb-authz"} {
		file := filepath.Join("registry", "components", name, "component.yaml")
		p.write(file, sunset.ReplaceAllString(strings.Replace(p.read(file), "stage: alpha\n", "stage: ga\n", 1), ""))
	}
	if err := os.CopyFS(p.dir, os.DirFS(filepath.Join(grpcFixture, "project"))); err != nil {
		t.Fatal(err)
	}
	p.write("go.mod", "module "+wiringModule+"\n\ngo 1.26.11\n")
	p.git("init", "-q")
	return p
}

// grpcCheckIDs are the checks of the generated service that TestRuntimeGRPC runs, each with the ID of the test record
// it proves.
var grpcCheckIDs = []string{
	"ST-SUP-RC07-OC19-T01 A gRPC call runs its command or query fill with the same handling as HTTP",
	"ST-SUP-RC33-OC08-T01 A stream query gets a gRPC stream and no HTTP route",
}

// TestRuntimeGRPC generates the gRPC test service with stego apply, reads its routes and its gRPC registration, builds
// it with the pinned Go, and runs the check of testdata/runtime_grpc/check in its module once against the local
// PostgreSQL server of STEGO_TEST_POSTGRES_DSN and a local SpiceDB container. A missing server fails the test: a
// missing result is not a pass.
func TestRuntimeGRPC(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a generated service")
	}
	// Not parallel: the check turns TLS on in the PostgreSQL server with a certificate of its own and resets it at its
	// end, as the check of TestRuntimeWiring does, so the two must not run at the same time. A test that is not
	// parallel ends before the parallel tests of the package go on.
	if os.Getenv("STEGO_TEST_POSTGRES_DSN") == "" {
		t.Fatal("STEGO_TEST_POSTGRES_DSN is not set: this test needs a local PostgreSQL server")
	}
	p := newGRPCProject(t, stegoBinary(t))
	p.stego("apply")
	t.Run("ST-SUP-RC33-OC08-T01 The stream query is marked from the proto file and gets no HTTP route", func(t *testing.T) {
		main, server, serve := p.read("out/main.go"), p.read("out/grpcapi/server.go"), p.read("out/queries/serve/serve.go")
		if strings.Contains(main, "/orders/watch") || strings.Contains(serve, `"GET /api/shop/v1/orders/watch"`) {
			t.Error("the stream query watch-orders has an HTTP route")
		}
		if !strings.Contains(main, `"GET /api/shop/v1/orders/{orderId}/summary"`) {
			t.Error("the unary query get-order-summary has no HTTP route")
		}
		for _, want := range []string{"queryserve.ServeWatchOrdersStream[", "pb0.RegisterShopServer(registrar, srv0)", "pb0.UnimplementedShopServer"} {
			if !strings.Contains(server, want) {
				t.Errorf("the gRPC registration lacks %q:\n%s", want, server)
			}
		}
		if !strings.Contains(serve, "query.RunStream(") || !strings.Contains(serve, "func ServeWatchOrdersStream[") {
			t.Error("the stream query is not served through its Stream function on query.RunStream")
		}
	})
	check, err := os.ReadFile(filepath.Join(grpcFixture, "check", "grpc_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	p.write("out/grpccheck/grpc_test.go", string(check))
	p.useLocalRuntime()
	p.goCommand("mod", "tidy")
	p.goCommand("vet", "./out/...")
	binary := filepath.Join(p.dir, "bin", "service")
	p.goCommand("build", "-o", binary, "./out")
	endpoint, key := startWiringSpiceDB(t)
	cmd := exec.Command(wiringPinnedGo(), "test", "-count=1", "-timeout=300s", "-v", "-run", "^TestGRPC$", "./out/grpccheck")
	cmd.Dir = p.dir
	cmd.Env = wiringGoEnv("STEGO_TEST_SPICEDB="+endpoint, "STEGO_TEST_SPICEDB_KEY="+key, "STEGO_GRPC_BINARY="+binary)
	output, runErr := cmd.CombinedOutput()
	for _, id := range grpcCheckIDs {
		t.Run(id, func(t *testing.T) {
			name := strings.ReplaceAll(id, " ", "_")
			if !strings.Contains(string(output), "--- PASS: TestGRPC/"+name+" ") {
				t.Fatalf("the check %s of the generated service did not run and pass (go test: %v):\n%s", strings.SplitN(id, " ", 2)[0], runErr, output)
			}
		})
	}
}
