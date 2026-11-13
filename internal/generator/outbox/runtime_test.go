package outbox

import (
	_ "embed"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/auditlog"
	"github.com/jsell-rh/stego/internal/generator/idempotency"
	"github.com/jsell-rh/stego/internal/types"
)

// The tests of the generated outbox wiring on the runtime. They are copied into the generated package and run there
// with the pinned Go SDK against the runtime module of this repository.
//
//go:embed testdata/runtime_test.go
var runtimeTests string

// The migration of the former outbox component, which STEGO no longer generates, for the test of an upgraded
// database (ST-SUP-RC18-OC30-T02).
//
//go:embed testdata/former.sql
var formerMigration string

// The output namespaces of the service of the RC18-OC30 test records.
const (
	outboxNamespace      = "internal/outbox"
	auditNamespace       = "internal/auditlog"
	idempotencyNamespace = "internal/idempotency"
	moduleName           = "example.com/orders"
	// kafkaNamespace is the package of the outbox wiring of the service with kafka-producer.
	kafkaNamespace = "internal/eventsoutbox"
)

// ordersContext is the declaration of the RC18-OC30 test records, as the generators read it: a rest-crud service
// orders on the runtime data path with the audit-log and idempotency components, the owned entity Order (fields
// id, sku and qty) and the collection orders with create, read, list, patch and delete.
func ordersContext(namespace string) gen.Context {
	return gen.Context{
		ModuleName:      moduleName,
		ServiceName:     "orders",
		OutputNamespace: namespace,
		ErrorTypeBase:   "https://errors.example.com/",
		ComponentConfig: map[string]any{"anchor_period": "1h", "retention_months": 13},
		PeerNamespaces: map[string]string{"outbox": outboxNamespace, "audit-log": auditNamespace,
			"idempotency": idempotencyNamespace, "postgres-adapter": "internal/storage", "tenancy": "internal/tenancy"},
		PortBindings: map[string]string{"audit-sink": "platform-audit-sink"},
		Idempotency:  &types.Idempotency{RequiredOn: []types.Operation{types.OpCreate}, Fingerprint: "body"},
		Entities: []types.Entity{{Name: "Order", Fields: []types.Field{
			{Name: "sku", Type: types.FieldTypeString}, {Name: "qty", Type: types.FieldTypeInt32}}}},
		Collections: []types.Collection{{Name: "orders", Entity: "Order", Patchable: []string{"qty"},
			Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpList, types.OpPatch, types.OpDelete}}},
	}
}

// generateOrders gives the files of the outbox, the audit-log and the idempotency components of the service.
func generateOrders(t *testing.T) (map[string][]gen.File, *gen.Wiring) {
	t.Helper()
	out := map[string][]gen.File{}
	var wiring *gen.Wiring
	for _, c := range []struct {
		name, namespace string
		generator       gen.Generator
	}{{"outbox", outboxNamespace, new(Generator)}, {"audit-log", auditNamespace, new(auditlog.Generator)},
		{"idempotency", idempotencyNamespace, new(idempotency.Generator)}} {
		files, w, err := c.generator.Generate(ordersContext(c.namespace))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if c.name == "outbox" {
			wiring = w
		}
		out[c.name] = files
	}
	return out, wiring
}

var queueTableDDL = regexp.MustCompile(`(?i)create\s+(table|schema)[^;]*stego_outbox`)

func TestRuntimeOutput(t *testing.T) {
	t.Run("ST-SUP-RC18-OC30-T01 One migration creates the queue table", func(t *testing.T) {
		files, wiring := generateOrders(t)
		var outboxPaths []string
		for component, list := range files {
			for _, f := range list {
				if queueTableDDL.Match(f.Bytes()) {
					t.Fatalf("the %s component emits %s, which creates the queue table or its schema itself", component, f.Path)
				}
			}
		}
		for _, f := range files["outbox"] {
			outboxPaths = append(outboxPaths, f.Path)
		}
		if got := strings.Join(outboxPaths, ","); got != outboxNamespace+"/"+runtimeFile {
			t.Fatalf("the outbox emits %s, want its wiring %s only", got, runtimeFile)
		}
		source := string(files["outbox"][0].Bytes())
		for _, want := range []string{`"github.com/jsell-rh/stego/registry/runtime/outbox"`, "outbox.Migration(relayRole, runtimeRole, workerRole)", "outbox.NewWorker(queue, config)"} {
			if !strings.Contains(source, want) {
				t.Fatalf("the outbox wiring holds no %s", want)
			}
		}
		// The runtime migration grants the roles; no grant of the outbox goes through the database access contract.
		if len(wiring.DatabaseAccess) != 0 || len(wiring.GoModRequires) != 0 {
			t.Fatalf("the outbox wiring asks for grants %v and modules %v, want none", wiring.DatabaseAccess, wiring.GoModRequires)
		}
		// A service with no entity, no command and no query has no command whose effects the outbox carries.
		noData := ordersContext(outboxNamespace)
		noData.Entities, noData.Collections = nil, nil
		if files, _, err := new(Generator).Generate(noData); err != nil || len(files) != 0 {
			t.Fatalf("a service with no data access gave %d files and %v, want none", len(files), err)
		}
	})
	t.Run("ST-SUP-RC18-OC30-T03 The roles get only the grants of the runtime migration and the tenant wall", func(t *testing.T) {
		_, wiring := generateOrders(t)
		for _, object := range wiring.DatabaseAccess {
			t.Fatalf("the outbox asks the database access contract for %v on %s.%s", object.Privileges, object.Schema, object.Name)
		}
	})
	t.Run("ST-SUP-RC18-OC30-T04 A message reaches the outside system through the destination that the wiring registers", func(t *testing.T) {
		plain := ordersContext(outboxNamespace)
		delete(plain.PeerNamespaces, "audit-log")
		files, _, err := new(Generator).Generate(plain)
		if err != nil {
			t.Fatal(err)
		}
		source := string(files[0].Bytes())
		if strings.Contains(source, "auditoutbox") || !strings.Contains(source, "var Destinations = []string{}") {
			t.Fatalf("a service without audit-log got an audit-sink destination:\n%s", source)
		}
	})
	t.Run("ST-SUP-RC18-OC31-T01 With kafka-producer, NewWorker registers the Kafka destination of the runtime with its settings and the log provider of the worker", func(t *testing.T) {
		ctx := ordersContext(outboxNamespace)
		ctx.PeerNamespaces[kafkaComponent] = "internal/events"
		files, wiring, err := new(Generator).Generate(ctx)
		if err != nil {
			t.Fatalf("the outbox with kafka-producer was refused: %v", err)
		}
		source := string(files[0].Bytes())
		for _, want := range []string{`"github.com/jsell-rh/stego/registry/runtime/outbox/kafkaoutbox"`,
			`var Destinations = []string{"audit-sink", "kafka"}`, "settings, err := kafkaoutbox.ReadSettings()",
			"kafka, err := kafkaoutbox.New(settings, config.Logger)", "worker.Register(kafka.Registration())"} {
			if !strings.Contains(source, want) {
				t.Errorf("the outbox wiring with kafka-producer holds no %s:\n%s", want, source)
			}
		}
		// The Kafka destination needs no grant and no module of its own: the runtime module requires its client.
		if len(wiring.DatabaseAccess) != 0 || len(wiring.GoModRequires) != 0 {
			t.Errorf("the outbox wiring with kafka-producer asks for grants %v and modules %v, want none", wiring.DatabaseAccess, wiring.GoModRequires)
		}
		plain, _, err := new(Generator).Generate(ordersContext(outboxNamespace))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(plain[0].Bytes()), "kafka") {
			t.Errorf("the outbox wiring without kafka-producer names Kafka:\n%s", plain[0].Bytes())
		}
	})
}

// service is a generated service module in a temporary folder, which builds against the runtime module of this
// repository.
type service struct {
	t   *testing.T
	dir string
}

func generateService(t *testing.T) *service {
	t.Helper()
	runtimeDir, err := filepath.Abs(filepath.Join("..", "..", "..", "registry", "runtime"))
	if err != nil {
		t.Fatal(err)
	}
	s := &service{t: t, dir: t.TempDir()}
	s.write("go.mod", "module "+moduleName+"\n\ngo 1.26.11\n\nrequire github.com/jsell-rh/stego/registry/runtime v0.0.0\n\nreplace github.com/jsell-rh/stego/registry/runtime => "+runtimeDir+"\n")
	sum, err := os.ReadFile(filepath.Join(runtimeDir, "go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	s.write("go.sum", string(sum))
	files, _ := generateOrders(t)
	for _, list := range files {
		for _, f := range list {
			s.write(f.Path, string(f.Bytes()))
		}
	}
	// The outbox wiring with kafka-producer, in a package of its own, so go vet builds it against the runtime too
	// (ST-SUP-RC18-OC31).
	withKafka := ordersContext(kafkaNamespace)
	withKafka.PeerNamespaces[kafkaComponent] = "internal/events"
	kafkaFiles, _, err := new(Generator).Generate(withKafka)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range kafkaFiles {
		s.write(f.Path, string(f.Bytes()))
	}
	s.write(filepath.Join(outboxNamespace, "runtime_test.go"), runtimeTests)
	// The migration of the former outbox component, for the test of an upgraded database.
	s.write(filepath.Join(outboxNamespace, "testdata", "former.sql"), formerMigration)
	return s
}

func (s *service) write(name, content string) {
	s.t.Helper()
	full := filepath.Join(s.dir, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		s.t.Fatal(err)
	}
}

// goTool runs the pinned Go SDK, STEGO_TEST_PINNED_GO, else the SDK of the test, in the module.
func (s *service) goTool(args ...string) ([]byte, error) {
	name := os.Getenv("STEGO_TEST_PINNED_GO")
	if name == "" {
		name = filepath.Join(runtime.GOROOT(), "bin", "go")
	}
	cmd := exec.Command(name, args...)
	cmd.Dir = s.dir
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod", "GOTOOLCHAIN=local")
	return cmd.CombinedOutput()
}

// run runs the test of the generated module whose subtest name starts with id, and fails when it fails, or when
// that subtest did not pass: a missing result is not a pass. A skip passes only when STEGO_REQUIRE_POSTGRES is
// not 1.
func (s *service) run(id string) {
	s.t.Helper()
	pattern := "^Test[A-Za-z]+$/^" + id + "(_|$)"
	out, err := s.goTool("test", "-count=1", "-timeout=120s", "-v", "-run", pattern, "./"+outboxNamespace)
	if err != nil {
		s.t.Fatalf("go test -run %s: %v\n%s", pattern, err, out)
	}
	text := string(out)
	passed := regexp.MustCompile(`--- PASS: Test[A-Za-z]+/` + id + `_`).MatchString(text)
	skipped := regexp.MustCompile(`--- SKIP: Test[A-Za-z]+/` + id + `_`).MatchString(text)
	switch {
	case passed:
	case skipped && os.Getenv("STEGO_REQUIRE_POSTGRES") != "1":
		s.t.Skipf("the generated test %s skipped: no PostgreSQL server", id)
	default:
		s.t.Fatalf("the generated test %s did not pass\n%s", id, out)
	}
	s.t.Log(text)
}

// TestGeneratedRuntimeOutbox builds the generated outbox wiring against the runtime module and runs the tests of
// the generated module against the PostgreSQL server that STEGO_TEST_POSTGRES_DSN names, a superuser URL.
func TestGeneratedRuntimeOutbox(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a generated module")
	}
	s := generateService(t)
	if out, err := s.goTool("vet", "./..."); err != nil {
		t.Fatalf("go vet of the generated module: %v\n%s", err, out)
	}
	for _, id := range []string{"ST-SUP-RC18-OC30-T01", "ST-SUP-RC18-OC30-T02", "ST-SUP-RC18-OC30-T03", "ST-SUP-RC18-OC30-T04", "ST-SUP-RC18-OC30-T07", "ST-SUP-RC18-OC11-T01"} {
		t.Run(id, func(t *testing.T) {
			s.t = t
			s.run(id)
		})
	}
}
