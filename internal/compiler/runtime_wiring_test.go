package compiler

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// The proofs of the runtime data path through the stego binary: the switch of the data path (ST-SUP-RC01-OC41), the
// examples on the store path, and a service generated from testdata/runtime_wiring/project/service.yaml whose
// migration job and binary run against a local PostgreSQL server and a local SpiceDB container.

// wiringFixture is the folder of the test service: its project files and the check that runs in its module.
const wiringFixture = "testdata/runtime_wiring"

// wiringModule is the module of the test service, which its fills import.
const wiringModule = "example.com/shop"

// wiringPinnedGo is the Go command that builds the stego binary and the generated service: STEGO_TEST_PINNED_GO, else
// bin/go of the SDK that runs the test.
func wiringPinnedGo() string {
	if name := os.Getenv("STEGO_TEST_PINNED_GO"); name != "" {
		return name
	}
	return filepath.Join(goruntime.GOROOT(), "bin", "go")
}

// wiringGoEnv is the environment of a go command and of the stego binary: no workspace, the pinned Go first on the
// path, and no toolchain switch.
func wiringGoEnv(extra ...string) []string {
	return append(append(os.Environ(), "GOWORK=off", "GOTOOLCHAIN=local",
		"PATH="+filepath.Dir(wiringPinnedGo())+string(os.PathListSeparator)+os.Getenv("PATH")), extra...)
}

var (
	wiringStegoOnce sync.Once
	wiringStego     string
	wiringStegoErr  error
)

// stegoBinary builds the stego binary of this commit once for the test binary.
func stegoBinary(t *testing.T) string {
	t.Helper()
	wiringStegoOnce.Do(func() {
		dir, err := os.MkdirTemp("", "stego-wiring-bin-")
		if err != nil {
			wiringStegoErr = err
			return
		}
		wiringStego = filepath.Join(dir, "stego")
		cmd := exec.Command(wiringPinnedGo(), "build", "-o", wiringStego, "./cmd/stego")
		cmd.Dir = filepath.Join("..", "..")
		cmd.Env = wiringGoEnv()
		if output, err := cmd.CombinedOutput(); err != nil {
			wiringStegoErr = &buildError{output: string(output), err: err}
		}
	})
	if wiringStegoErr != nil {
		t.Fatalf("building stego: %v", wiringStegoErr)
	}
	return wiringStego
}

type buildError struct {
	output string
	err    error
}

func (e *buildError) Error() string { return e.err.Error() + "\n" + e.output }

// wiringProject is one project folder of the test service with a registry copy of its own.
type wiringProject struct {
	t   *testing.T
	dir string
}

// runtimeArchetype is the components list of the rest-crud archetype of the registry copy on the runtime data path:
// the shipped list with the command and query generators and the tenancy, domain-probe and outbox components, and
// without tsl-search, which the runtime data path does not serve.
const runtimeArchetype = "components:\n  - rest-api\n  - postgres-adapter\n  - tenancy\n  - domain-probe\n  - command\n  - query\n  - outbox\n  - otel-tracing\n  - health-check\n"

// storeArchetype is runtimeArchetype without the command and query generators: a service on the GORM store.
const storeArchetype = "components:\n  - rest-api\n  - postgres-adapter\n  - tenancy\n  - domain-probe\n  - outbox\n  - otel-tracing\n  - health-check\n"

// newWiringProject copies the registry of this repository with the components of the archetype given, each alpha
// component of the test staged as ga, and the project files of the fixture into a new folder.
func newWiringProject(t *testing.T, components string) *wiringProject {
	t.Helper()
	p := &wiringProject{t: t, dir: t.TempDir()}
	if err := os.CopyFS(filepath.Join(p.dir, "registry"), os.DirFS(filepath.Join("..", "..", "registry"))); err != nil {
		t.Fatal(err)
	}
	p.archetype(components)
	sunset := regexp.MustCompile(`sunset: "?[0-9-]+"?\n`)
	for _, name := range []string{"tenancy", "command", "query", "domain-probe", "audit-log", "idempotency", "spicedb-authz"} {
		file := filepath.Join(p.dir, "registry", "components", name, "component.yaml")
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		p.write(filepath.Join("registry", "components", name, "component.yaml"),
			sunset.ReplaceAllString(strings.Replace(string(data), "stage: alpha\n", "stage: ga\n", 1), ""))
	}
	if err := os.CopyFS(p.dir, os.DirFS(filepath.Join(wiringFixture, "project"))); err != nil {
		t.Fatal(err)
	}
	p.write("go.mod", "module "+wiringModule+"\n\ngo 1.26.10\n")
	return p
}

// archetype sets the components of the rest-crud archetype of the registry copy.
func (p *wiringProject) archetype(components string) {
	p.t.Helper()
	file := filepath.Join(p.dir, "registry", "archetypes", "rest-crud", "archetype.yaml")
	data, err := os.ReadFile(file)
	if err != nil {
		p.t.Fatal(err)
	}
	text := regexp.MustCompile(`(?s)components:\n(  - [a-z-]+\n)+`).ReplaceAllString(string(data), components)
	p.write(filepath.Join("registry", "archetypes", "rest-crud", "archetype.yaml"), text)
}

func (p *wiringProject) write(name, text string) {
	p.t.Helper()
	full := filepath.Join(p.dir, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		p.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(text), 0o644); err != nil {
		p.t.Fatal(err)
	}
}

func (p *wiringProject) read(name string) string {
	p.t.Helper()
	data, err := os.ReadFile(filepath.Join(p.dir, name))
	if err != nil {
		p.t.Fatal(err)
	}
	return string(data)
}

// stego runs the stego binary in the project with its registry copy.
func (p *wiringProject) stego(args ...string) string {
	p.t.Helper()
	cmd := exec.Command(stegoBinary(p.t), args...)
	cmd.Dir = p.dir
	cmd.Env = wiringGoEnv("STEGO_REGISTRY="+filepath.Join(p.dir, "registry"), "STEGO_MODULE="+wiringModule)
	output, err := cmd.CombinedOutput()
	if err != nil {
		p.t.Fatalf("stego %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return string(output)
}

// goCommand runs the pinned go command in the project.
func (p *wiringProject) goCommand(args ...string) {
	p.t.Helper()
	cmd := exec.Command(wiringPinnedGo(), args...)
	cmd.Dir = p.dir
	cmd.Env = wiringGoEnv("GOFLAGS=-mod=mod")
	if output, err := cmd.CombinedOutput(); err != nil {
		p.t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}

// useLocalRuntime makes the module of the project take the runtime library from this repository's registry/runtime
// folder, as the work file of scripts/runtime-workspace.sh does for a local build.
func (p *wiringProject) useLocalRuntime() {
	p.t.Helper()
	runtimeDir, err := filepath.Abs(filepath.Join("..", "..", "registry", "runtime"))
	if err != nil {
		p.t.Fatal(err)
	}
	p.write("go.mod", p.read("go.mod")+"\nreplace stego.dev/runtime => "+runtimeDir+"\n")
}

// planned gives the paths that a plan output lists with the action given.
func planned(output, action string) map[string]bool {
	paths := map[string]bool{}
	for _, line := range strings.Split(output, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), action+":"); ok {
			paths[strings.TrimSpace(rest)] = true
		}
	}
	return paths
}

func TestRuntimeDataPath(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the stego binary")
	}
	t.Run("ST-SUP-RC01-OC41-T05 The runtime data path is switched on only for a service with no record at its base ref", func(t *testing.T) {
		fresh := newWiringProject(t, runtimeArchetype)
		generated := planned(fresh.stego("plan"), "generate")
		for _, want := range []string{RuntimeWiringNamespace + "/runtime.go", "internal/storage/tables.go", "commands/handlers/handlers.go", "queries/serve/serve.go"} {
			if !generated[want] {
				t.Errorf("the plan of the service with no record does not generate %s", want)
			}
		}
		for _, refused := range []string{"internal/storage/store.go", "internal/storage/models.go"} {
			if generated[refused] {
				t.Errorf("the plan of the service with no record generates the store file %s", refused)
			}
		}

		// The release on the GORM store: the same declaration without its commands and queries, generated with no
		// command and no query generator. Then the declaration and the archetype gain them.
		released := newWiringProject(t, storeArchetype)
		full := released.read("service.yaml")
		store := full[:strings.Index(full, "commands:")]
		store = strings.Replace(store, "  command:\n    proto_files:\n      - {path: proto/shop.proto, import_path: shop/v1/shop.proto}\n", "", 1)
		released.write("service.yaml", store)
		if err := os.RemoveAll(filepath.Join(released.dir, "fills")); err != nil {
			t.Fatal(err)
		}
		released.stego("apply")
		if !strings.Contains(released.read(".stego/state.yaml"), "internal/storage/store.go") {
			t.Fatal("the release on the GORM store records no store file")
		}
		released.write("service.yaml", full)
		if err := os.CopyFS(filepath.Join(released.dir, "fills"), os.DirFS(filepath.Join(wiringFixture, "project", "fills"))); err != nil {
			t.Fatal(err)
		}
		released.archetype(runtimeArchetype)
		released.useLocalRuntime()
		released.goCommand("mod", "download")
		output := released.stego("plan")
		for _, refused := range []string{RuntimeWiringNamespace + "/runtime.go", "internal/storage/tables.go"} {
			if planned(output, "generate")[refused] {
				t.Errorf("the plan of the service with a release on the GORM store generates %s:\n%s", refused, output)
			}
		}
		if planned(output, "delete")["internal/storage/store.go"] || !planned(output, "generate")["commands/handlers/handlers.go"] {
			t.Errorf("the plan of the service with a release on the GORM store does not keep the store and add the commands:\n%s", output)
		}
	})

	t.Run("ST-SUP-RC01-OC41-T06 An example on the GORM store keeps its generated output", func(t *testing.T) {
		for _, example := range []string{"user-management", "user-management-rhsso"} {
			p := &wiringProject{t: t, dir: t.TempDir()}
			if err := os.CopyFS(p.dir, os.DirFS(filepath.Join("..", "..", "examples", example))); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(stegoBinary(t), "plan")
			cmd.Dir = p.dir
			registryDir, err := filepath.Abs(filepath.Join("..", "..", "registry"))
			if err != nil {
				t.Fatal(err)
			}
			cmd.Env = wiringGoEnv("STEGO_REGISTRY=" + registryDir)
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("stego plan of %s: %v\n%s", example, err, output)
			}
			if !strings.Contains(string(output), "Summary: 0 to generate, 0 to update, 0 to delete") {
				t.Errorf("the plan of the example %s changes its generated output:\n%s", example, output)
			}
			if strings.Contains(p.read(".stego/plan.yaml"), RuntimeWiringNamespace) {
				t.Errorf("the plan of the example %s holds the runtime wiring", example)
			}
		}
	})
}

// wiringSpiceDBImage is the pinned SpiceDB image of the local stand-in (ST-STD-LOCAL-R04), as the runtime's tests pin
// it.
const wiringSpiceDBImage = "authzed/spicedb:v1.57.0@sha256:a170d372c9f6b921bb6849064df6ca3566dd5b174882c1a2c4300c2ae1d60efd"

// startWiringSpiceDB starts a local SpiceDB container without privilege, with an in-memory datastore and limits, and
// gives its endpoint and preshared key. It removes the container when the test ends.
func startWiringSpiceDB(t *testing.T) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatal("docker is not on the PATH: this test needs a local SpiceDB container")
	}
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	name, key := "stego-wiring-spicedb-"+hex.EncodeToString(suffix), "wiring-"+hex.EncodeToString(suffix)
	run := exec.Command("docker", "run", "-d", "--rm", "--name", name, "--memory", "256m", "--cpus", "1",
		"--security-opt", "no-new-privileges", "--cap-drop", "ALL", "--stop-timeout", "1",
		"-p", "127.0.0.1::50051", wiringSpiceDBImage, "serve", "--grpc-preshared-key", key, "--datastore-engine", "memory")
	if output, err := run.CombinedOutput(); err != nil {
		t.Fatalf("docker run SpiceDB: %v\n%s", err, output)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		out, err := exec.Command("docker", "port", name, "50051/tcp").Output()
		if err == nil && len(out) > 0 {
			return strings.TrimSpace(strings.Split(string(out), "\n")[0]), key
		}
	}
	t.Fatal("the SpiceDB container publishes no port")
	return "", ""
}

// wiringCheckIDs are the checks of the generated service that TestRuntimeWiring runs, each with the ID of the test
// record it proves.
var wiringCheckIDs = []string{
	"ST-SUP-RC01-OC21-T03 The generated migration job runs as the owner role and the service as the runtime role",
	"ST-SUP-RC29-OC23-T02 The generated migration job writes the schema to SpiceDB before the service starts",
	"ST-SUP-RC20-OC37-T07 A collection create and read reach the generated handlers through the generated main.go",
	"ST-SUP-RC32-OC02-T03 A declared command reaches its fill through the generated main.go",
	"ST-SUP-RC33-OC02-T04 A declared query reaches its fill through the generated main.go",
}

// TestRuntimeWiring generates the test service with stego apply, builds it with the pinned Go, and runs the check of
// testdata/runtime_wiring/check in its module once: the check runs the migration job and the service binary against
// the local PostgreSQL server of STEGO_TEST_POSTGRES_DSN and a local SpiceDB container, and sends one collection
// call, one command and one query over HTTP. A missing server fails the test: a missing result is not a pass.
func TestRuntimeWiring(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a generated service")
	}
	if os.Getenv("STEGO_TEST_POSTGRES_DSN") == "" {
		t.Fatal("STEGO_TEST_POSTGRES_DSN is not set: this test needs a local PostgreSQL server")
	}
	p := newWiringProject(t, runtimeArchetype)
	p.stego("apply")
	check, err := os.ReadFile(filepath.Join(wiringFixture, "check", "wiring_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	p.write("out/wiringcheck/wiring_test.go", string(check))
	p.useLocalRuntime()
	p.goCommand("mod", "tidy")
	p.goCommand("vet", "./out/...")
	binary := filepath.Join(p.dir, "bin", "service")
	p.goCommand("build", "-o", binary, "./out")
	endpoint, key := startWiringSpiceDB(t)
	cmd := exec.Command(wiringPinnedGo(), "test", "-count=1", "-timeout=300s", "-v", "-run", "^TestWiring$", "./out/wiringcheck")
	cmd.Dir = p.dir
	cmd.Env = wiringGoEnv("STEGO_TEST_SPICEDB="+endpoint, "STEGO_TEST_SPICEDB_KEY="+key, "STEGO_WIRING_BINARY="+binary)
	output, runErr := cmd.CombinedOutput()
	for _, id := range wiringCheckIDs {
		t.Run(id, func(t *testing.T) {
			name := strings.ReplaceAll(id, " ", "_")
			if !strings.Contains(string(output), "--- PASS: TestWiring/"+name+" ") {
				t.Fatalf("the check %s of the generated service did not run and pass (go test: %v):\n%s", strings.SplitN(id, " ", 2)[0], runErr, output)
			}
		})
	}
}
