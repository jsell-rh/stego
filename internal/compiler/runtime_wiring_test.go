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
	"testing"
	"time"
)

// The proofs of the runtime data path through the stego binary: a service with data access on the runtime data path
// only (ST-SUP-RC01-OC41, ST-SUP-RC20-OC39), the examples on it, and a service generated from testdata/runtime_wiring/project/service.yaml whose
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

// stegoFixture is the stego binary of this commit, which the tests share.
var stegoFixture SharedFixture

// stegoBinary builds the stego binary of this commit once per test run and gives its path.
func stegoBinary(t *testing.T) string {
	t.Helper()
	return filepath.Join(stegoFixture.Dir(t, func(t *testing.T, dir string) {
		cmd := exec.Command(wiringPinnedGo(), "build", "-o", filepath.Join(dir, "stego"), "./cmd/stego")
		cmd.Dir = filepath.Join("..", "..")
		cmd.Env = wiringGoEnv()
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("building stego: %v\n%s", err, output)
		}
	}), "stego")
}

// wiringProject is one project folder of the test service with a registry copy of its own.
type wiringProject struct {
	t      *testing.T
	dir    string
	binary string
}

// runtimeArchetype is the components list of the rest-crud archetype of the registry copy on the runtime data path:
// the shipped list with the tenancy component.
const runtimeArchetype = "components:\n  - rest-api\n  - postgres-adapter\n  - tenancy\n  - domain-probe\n  - command\n  - query\n  - outbox\n  - otel-tracing\n  - health-check\n"

// noQueryArchetype is runtimeArchetype without the query generator.
const noQueryArchetype = "components:\n  - rest-api\n  - postgres-adapter\n  - tenancy\n  - domain-probe\n  - command\n  - outbox\n  - otel-tracing\n  - health-check\n"

// storeArchetype is runtimeArchetype without the command and query generators: the component set of a service on the
// GORM store, which is refused now (ST-SUP-RC20-OC39).
const storeArchetype = "components:\n  - rest-api\n  - postgres-adapter\n  - tenancy\n  - domain-probe\n  - outbox\n  - otel-tracing\n  - health-check\n"

// newWiringProject copies the registry of this repository with the components of the archetype given, each alpha
// component of the test staged as ga, and the project files of the fixture into a new folder.
func newWiringProject(t *testing.T, binary, components string) *wiringProject {
	t.Helper()
	p := &wiringProject{t: t, dir: t.TempDir(), binary: binary}
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
	p.write("go.mod", "module "+wiringModule+"\n\ngo 1.26.11\n")
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
	output, err := p.stegoRun(args...)
	if err != nil {
		p.t.Fatalf("stego %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return output
}

// stegoRun runs the stego binary in the project with its registry copy and gives its output and its error.
func (p *wiringProject) stegoRun(args ...string) (string, error) {
	p.t.Helper()
	cmd := exec.Command(p.binary, args...)
	cmd.Dir = p.dir
	cmd.Env = wiringGoEnv("STEGO_REGISTRY="+filepath.Join(p.dir, "registry"), "STEGO_MODULE="+wiringModule)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

// git runs git in the project.
func (p *wiringProject) git(args ...string) {
	p.t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=wiring", "-c", "user.email=wiring@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = p.dir
	if output, err := cmd.CombinedOutput(); err != nil {
		p.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
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
	replace := "replace github.com/jsell-rh/stego/registry/runtime => " + runtimeDir
	if mod := p.read("go.mod"); !strings.Contains(mod, replace) {
		p.write("go.mod", mod+"\n"+replace+"\n")
	}
}

// afterSlotBinding binds the fill order-created of testdata/runtime_wiring/after at orders.after_create.
const afterSlotBinding = "slots:\n  - collection: orders\n    slot: after_create\n    fan-out:\n      - order-created\n"

// applyWithAfterSlot applies the project with the after slot of orders bound to a qualified fill, as a service
// author does (ST-EXT-UC02): a first apply writes the slot contracts with no binding, stego test records the fill's
// evidence, a key that SIGNERS lists signs it, and a second apply binds and wires it.
func (p *wiringProject) applyWithAfterSlot() {
	t := p.t
	t.Helper()
	if err := os.CopyFS(filepath.Join(p.dir, "fills"), os.DirFS(filepath.Join(wiringFixture, "after", "fills"))); err != nil {
		t.Fatal(err)
	}
	p.stego("apply")
	p.useLocalRuntime()
	p.goCommand("mod", "tidy")
	keys := t.TempDir()
	key := filepath.Join(keys, "signer")
	if output, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "signer", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, output)
	}
	public, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(public))
	p.write("SIGNERS", "signer@example.com "+fields[0]+" "+fields[1]+"\n")
	home := filepath.Join(keys, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{"gpg.format", "ssh"}, {"user.signingkey", key}} {
		cmd := exec.Command("git", "config", "--global", pair[0], pair[1])
		cmd.Env = append(os.Environ(), "HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"))
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git config: %v\n%s", err, output)
		}
	}
	signed := func(args ...string) string {
		cmd := exec.Command(p.binary, args...)
		cmd.Dir = p.dir
		cmd.Env = wiringGoEnv("STEGO_REGISTRY="+filepath.Join(p.dir, "registry"), "STEGO_MODULE="+wiringModule, "GOFLAGS=-mod=mod",
			"HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"))
		output, _ := cmd.CombinedOutput()
		return string(output)
	}
	if output := signed("test"); !strings.Contains(output, "fill order-created: tests passed") {
		t.Fatalf("stego test did not record the evidence of order-created:\n%s", output)
	}
	if output := signed("fill", "sign", "order-created"); !strings.Contains(output, "signed_at") {
		t.Fatalf("stego fill sign order-created:\n%s", output)
	}
	declaration := p.read("service.yaml")
	p.write("service.yaml", strings.Replace(declaration, "commands:\n", afterSlotBinding+"commands:\n", 1))
	p.stego("apply")
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
	t.Parallel()
	stego := stegoBinary(t)
	t.Run("ST-SUP-RC01-OC41-T05 A service with data access is generated on the runtime data path only, and is refused without the command and query components", func(t *testing.T) {
		fresh := newWiringProject(t, stego, runtimeArchetype)
		generated := planned(fresh.stego("plan"), "generate")
		for _, want := range []string{RuntimeWiringNamespace + "/runtime.go", "internal/storage/tables.go", "commands/handlers/handlers.go", "queries/serve/serve.go"} {
			if !generated[want] {
				t.Errorf("the plan of the service does not generate %s", want)
			}
		}
		for _, refused := range []string{"internal/storage/store.go", "internal/storage/models.go"} {
			if generated[refused] {
				t.Errorf("the plan of the service generates the store file %s", refused)
			}
		}
		fresh.stego("apply")
		record := fresh.read(".stego/state.yaml")
		if !strings.Contains(record, "format_version: 5") || strings.Contains(record, "data_path") {
			t.Errorf("the record is not of format version 5 without a data path:\n%s", record)
		}

		// The same declaration without the command or the query generator: no component serves its data, so the
		// components that served it on the GORM store refuse the run, and the fault names ST-SUP-RC20-OC39.
		for _, components := range []string{storeArchetype, noQueryArchetype} {
			lacking := newWiringProject(t, stego, components)
			if components == storeArchetype {
				// The declaration of a release on the GORM store: its collections and entities, with no command, no
				// query and no setting of the command component.
				full := lacking.read("service.yaml")
				store := full[:strings.Index(full, "commands:")]
				store = strings.Replace(store, "  command:\n    proto_files:\n      - {path: proto/shop.proto, import_path: shop/v1/shop.proto}\n", "", 1)
				lacking.write("service.yaml", store)
				if err := os.RemoveAll(filepath.Join(lacking.dir, "fills")); err != nil {
					t.Fatal(err)
				}
			}
			output, err := lacking.stegoRun("plan")
			if err == nil || !strings.Contains(output, "ST-SUP-RC20-OC39") || !strings.Contains(output, "command") || !strings.Contains(output, "query component") {
				t.Errorf("the plan of a service with data access and without the command and query generators gave %v, want a refusal that names ST-SUP-RC20-OC39:\n%s", err, output)
			}
			if _, err := os.Stat(filepath.Join(lacking.dir, "out")); !os.IsNotExist(err) {
				t.Errorf("the refused plan wrote the output folder: %v", err)
			}
		}
	})

	t.Run("ST-SUP-RC11-OC31-T01 A service on the runtime data path gets no jwt-auth middleware and its verifier takes the declared settings", func(t *testing.T) {
		runtime := newWiringProject(t, stego, runtimeArchetype)
		runtime.stego("apply")
		if _, err := os.Stat(filepath.Join(runtime.dir, "out", "internal", "auth")); !os.IsNotExist(err) {
			t.Errorf("the service on the runtime data path has the jwt-auth package out/internal/auth: %v", err)
		}
		main := runtime.read("out/main.go")
		if strings.Contains(main, "/out/internal/auth\"") || strings.Contains(main, "NewAuthMiddleware") {
			t.Error("the main.go of the service on the runtime data path wires the jwt-auth middleware")
		}
		if !strings.Contains(main, "service.Middleware(") {
			t.Error("the main.go of the service on the runtime data path does not wrap the routes in the runtime verifier")
		}
		wiring := runtime.read("out/" + RuntimeWiringNamespace + "/runtime.go")
		for _, want := range []string{`declaredIssuer    = "https://issuer.wiring.test/realms/r1"`, `declaredAudience  = "wiring-shop"`} {
			if !strings.Contains(wiring, want) {
				t.Errorf("the runtime verifier does not take the declared setting %s", want)
			}
		}
	})

	t.Run("ST-SUP-RC01-OC41-T06 An example on the runtime data path keeps its generated output", func(t *testing.T) {
		for _, example := range []string{"user-management", "user-management-rhsso"} {
			p := &wiringProject{t: t, dir: t.TempDir()}
			if err := os.CopyFS(p.dir, os.DirFS(filepath.Join("..", "..", "examples", example))); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(stego, "plan")
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
			if record := p.read(".stego/state.yaml"); !strings.Contains(record, "format_version: 5") || strings.Contains(record, "data_path") {
				t.Errorf("the record of the example %s is not of format version 5 without a data path", example)
			}
			if _, err := os.Stat(filepath.Join(p.dir, "out", "internal", "storage", "store.go")); err == nil {
				t.Errorf("the example %s holds the GORM store", example)
			}
			// An apply writes the record again, with the compiler of this commit: it states the runtime data path still.
			apply := exec.Command(stego, "apply")
			apply.Dir, apply.Env = p.dir, cmd.Env
			if output, err := apply.CombinedOutput(); err != nil {
				t.Fatalf("stego apply of %s: %v\n%s", example, err, output)
			}
			if record := p.read(".stego/state.yaml"); strings.Contains(record, "data_path") || !strings.Contains(record, RuntimeWiringNamespace) {
				t.Errorf("the record that apply writes for the example %s states a data path or lacks the runtime wiring", example)
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
	"ST-SUP-RC01-OC41-T04 The runtime pool is opened through tenant.Open with the adapter's verified TLS configuration",
	"ST-SUP-RC01-OC41-T07 The generated migration job runs again on its own database and refuses a database with tables it did not make",
	"ST-SUP-RC29-OC23-T02 The generated migration job writes the schema to SpiceDB before the service starts",
	"ST-SUP-RC20-OC37-T07 A collection create and read reach the generated handlers through the generated main.go",
	"ST-SUP-RC32-OC02-T03 A declared command reaches its fill through the generated main.go",
	"ST-SUP-RC33-OC02-T04 A declared query reaches its fill through the generated main.go",
	"ST-SUP-RP14-LC04-T02 The generated service runs its workers through the service runtime",
	"ST-SUP-RP14-LC04-T03 The generated worker delivers each audit entry and anchor to the default table",
	"ST-SUP-RC18-OC31-T02 The generated worker sends to Kafka with the settings of the process, and does not start without them",
	"ST-SUP-RC20-OC40-T02 A change event of a generated write arrives once with its key and headers, waits out a broker outage in the outbox, and keeps the order of its entity",
}

// TestRuntimeWiring generates the test service with stego apply, builds it with the pinned Go, and runs the check of
// testdata/runtime_wiring/check in its module once: the check runs the migration job and the service binary against
// the local PostgreSQL server of STEGO_TEST_POSTGRES_DSN, a local SpiceDB container and the local Kafka broker of
// kafkatest, and sends collection calls, one command and one query over HTTP. A missing server fails the test: a
// missing result is not a pass.
func TestRuntimeWiring(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a generated service")
	}
	t.Parallel()
	if os.Getenv("STEGO_TEST_POSTGRES_DSN") == "" {
		t.Fatal("STEGO_TEST_POSTGRES_DSN is not set: this test needs a local PostgreSQL server")
	}
	stego := stegoBinary(t)
	p := newWiringProject(t, stego, runtimeArchetype)
	p.applyWithAfterSlot()
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
	// The next release adds the entity Tag with no schema step.
	next := newWiringProject(t, stego, runtimeArchetype)
	declaration := next.read("service.yaml")
	next.write("service.yaml", strings.Replace(declaration, "entities:\n", "entities:\n  - name: Tag\n    fields:\n      - {name: label, type: string}\n", 1))
	next.stego("apply")
	next.useLocalRuntime()
	next.goCommand("mod", "tidy")
	nextBinary := filepath.Join(next.dir, "bin", "service")
	next.goCommand("build", "-o", nextBinary, "./out")
	endpoint, key := startWiringSpiceDB(t)
	cmd := exec.Command(wiringPinnedGo(), "test", "-count=1", "-timeout=480s", "-v", "-run", "^TestWiring$", "./out/wiringcheck")
	cmd.Dir = p.dir
	cmd.Env = wiringGoEnv("STEGO_TEST_SPICEDB="+endpoint, "STEGO_TEST_SPICEDB_KEY="+key, "STEGO_WIRING_BINARY="+binary,
		"STEGO_WIRING_NEXT_BINARY="+nextBinary)
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
