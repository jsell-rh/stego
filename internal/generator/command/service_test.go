package command

import (
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/postgresadapter"
)

//go:embed testdata/command_test.go
var serviceChecks []byte

// serviceFill is the fill reserve-and-charge of the generated service, which the service writes at
// fills/reserve-and-charge/fill.go.
var serviceFill = []byte(`// Package reservecharge is the fill reserve-and-charge of the generated test service: it reserves the stock of an
// order and records a charge, through its command handle only. It imports the command slot contract and the
// standard library fill allow list only (ST-EXT-UC01-R03).
package reservecharge

import (
	"context"
	"encoding/json"

	commands "example.com/orders/out/commands"
)

// Behavior is what the fill does; a test of the service changes it.
var Behavior commands.ReserveAndChargeFill = ReserveAndCharge

// New gives the fill of the command reserve-and-charge.
func New() commands.ReserveAndChargeFill {
	return func(ctx context.Context, request *commands.ReserveAndChargeRequest, h *commands.ReserveAndChargeHandle) (*commands.ReserveAndChargeResponse, error) {
		return Behavior(ctx, request, h)
	}
}

// ReserveAndCharge writes one Reservation and one Charge of the order, stages one billing message for the charge
// and counts the reservation. It refuses more than ten items as short stock.
func ReserveAndCharge(ctx context.Context, request *commands.ReserveAndChargeRequest, h *commands.ReserveAndChargeHandle) (*commands.ReserveAndChargeResponse, error) {
	if request.Qty > 10 {
		return nil, commands.ReserveAndChargeShortStock
	}
	reservations, err := commands.ReservationRepository(h)
	if err != nil {
		return nil, err
	}
	reservation, err := reservations.Create(ctx, commands.Reservation{OrderID: request.OrderId, Sku: request.Sku, Qty: request.Qty}, nil)
	if err != nil {
		return nil, err
	}
	charges, err := commands.ChargeRepository(h)
	if err != nil {
		return nil, err
	}
	charge, err := charges.Create(ctx, commands.Charge{OrderID: request.OrderId, AmountCents: request.AmountCents, CardLast4: request.CardLast4}, nil)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(map[string]any{"charge": charge.ID(), "amount_cents": request.AmountCents})
	if err != nil {
		return nil, err
	}
	if err := h.Effects().Stage(ctx, "billing", "charge-requested", charge, payload); err != nil {
		return nil, err
	}
	if request.Qty > 5 {
		h.Probe().ReservationsMade("large")
	} else {
		h.Probe().ReservationsMade("small")
	}
	return &commands.ReserveAndChargeResponse{ReservationId: reservation.ID(), ChargeId: charge.ID()}, nil
}
`)

// spiceDBImage is the pinned SpiceDB image of the local stand-in (ST-STD-LOCAL-R04), as the runtime's tests pin it.
const spiceDBImage = "authzed/spicedb:v1.57.0@sha256:a170d372c9f6b921bb6849064df6ca3566dd5b174882c1a2c4300c2ae1d60efd"

// startSpiceDB starts a local SpiceDB container without privilege, with an in-memory datastore and limits, and gives
// its endpoint and preshared key. It removes the container when the test ends.
func startSpiceDB(t *testing.T) (string, string) {
	t.Helper()
	if endpoint := os.Getenv("STEGO_TEST_SPICEDB"); endpoint != "" {
		return endpoint, os.Getenv("STEGO_TEST_SPICEDB_KEY")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatal("docker is not on the PATH: this test needs a local SpiceDB container")
	}
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	name, key := "stego-command-spicedb-"+hex.EncodeToString(suffix), "command-"+hex.EncodeToString(suffix)
	run := exec.Command("docker", "run", "-d", "--rm", "--name", name, "--memory", "256m", "--cpus", "1",
		"--security-opt", "no-new-privileges", "--cap-drop", "ALL", "--stop-timeout", "1",
		"-p", "127.0.0.1::50051", spiceDBImage, "serve", "--grpc-preshared-key", key, "--datastore-engine", "memory")
	if output, err := run.CombinedOutput(); err != nil {
		t.Fatalf("docker run SpiceDB: %v\n%s", err, output)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	var port string
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		out, err := exec.Command("docker", "port", name, "50051/tcp").Output()
		if err == nil && len(out) > 0 {
			port = strings.TrimSpace(strings.Split(string(out), "\n")[0])
			break
		}
	}
	if port == "" {
		t.Fatal("the SpiceDB container publishes no port")
	}
	return port, key
}

// tablesMigration is the migration 001_initial that the PostgreSQL adapter creates for the declaration on the
// runtime data path (ST-SUP-RC01-OC41).
func tablesMigration(t *testing.T, ctx gen.Context) []byte {
	t.Helper()
	adapter := gen.Context{OutputNamespace: "storage", ModuleName: ctx.ModuleName, ServiceName: ctx.ServiceName, RuntimeData: true,
		Tenancy: ctx.Tenancy, Entities: ctx.Entities, PeerNamespaces: map[string]string{"postgres-adapter": "storage", gen.TenancyComponent: "tenancy"},
		ComponentConfig: map[string]any{"schema_generation": "tables-v1", "writer_lease": "off"}}
	files, _, err := new(postgresadapter.Generator).Generate(adapter)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f.Path == "storage/migrations/001_initial.sql" {
			return f.Content
		}
	}
	t.Fatal("the PostgreSQL adapter gave no migration 001_initial")
	return nil
}

// commandService generates the test service with its fill and its checks, once for the test binary.
func commandService(t *testing.T) *project {
	t.Helper()
	ctx := commandContext(t, declaration(t), commandSlot)
	return newProject(t, ctx, map[string][]byte{
		"fills/reserve-and-charge/fill.go": serviceFill,
		"check/command_test.go":            serviceChecks,
		"check/tables.sql":                 tablesMigration(t, ctx),
	})
}

// checkRun is the one run of the checks of the generated service: one go test of the check package with every
// check, so the service is built and its test binary run once.
type checkRun struct {
	once   sync.Once
	output string
	err    error
}

// runCheck tells whether the check id of the generated service ran and passed, against the local PostgreSQL server
// of STEGO_TEST_POSTGRES_DSN and a local SpiceDB container. The first call runs every check. A missing server
// fails the test: a missing result is not a pass.
func runCheck(t *testing.T, p *project, run *checkRun, endpoint, key, id string) {
	t.Helper()
	if os.Getenv("STEGO_TEST_POSTGRES_DSN") == "" {
		t.Fatal("STEGO_TEST_POSTGRES_DSN is not set: this test needs a local PostgreSQL server")
	}
	run.once.Do(func() {
		cmd := exec.Command(pinnedGo(), "test", "-count=1", "-timeout=300s", "-v", "-run", "^TestCommand$", "./check")
		cmd.Dir = p.dir
		cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod", "GOTOOLCHAIN=local", "STEGO_TEST_SPICEDB="+endpoint, "STEGO_TEST_SPICEDB_KEY="+key)
		output, err := cmd.CombinedOutput()
		run.output, run.err = string(output), err
	})
	if !strings.Contains(run.output, "--- PASS: TestCommand/"+id+"_") {
		t.Fatalf("the check %s of the generated service did not run and pass (go test: %v):\n%s", id, run.err, run.output)
	}
}

// TestCommandService generates a service with the command reserve-and-charge, builds it with the pinned Go and runs
// its HTTP and gRPC handlers against PostgreSQL and SpiceDB.
func TestCommandService(t *testing.T) {
	p := commandService(t)
	p.mustGo("vet", "./...")
	endpoint, key := startSpiceDB(t)
	run := &checkRun{}
	t.Run("ST-SUP-RC32-OC04-T01 The fill runs in one transaction with the caller's tenant", func(t *testing.T) { runCheck(t, p, run, endpoint, key, "ST-SUP-RC32-OC04-T01") })
	t.Run("ST-SUP-RC32-OC04-T02 An unauthenticated command never reaches the fill", func(t *testing.T) { runCheck(t, p, run, endpoint, key, "ST-SUP-RC32-OC04-T02") })
	t.Run("ST-SUP-RC32-OC05-T01 A fill error leaves nothing behind", func(t *testing.T) { runCheck(t, p, run, endpoint, key, "ST-SUP-RC32-OC05-T01") })
	t.Run("ST-SUP-RC32-OC05-T02 A successful command commits its writes and effects together", func(t *testing.T) { runCheck(t, p, run, endpoint, key, "ST-SUP-RC32-OC05-T02") })
	t.Run("ST-SUP-RC32-OC06-T01 A denied command answers 403 and does not call the fill", func(t *testing.T) { runCheck(t, p, run, endpoint, key, "ST-SUP-RC32-OC06-T01") })
	t.Run("ST-SUP-RC32-OC06-T02 A write under a parent that the principal may not act on is refused", func(t *testing.T) { runCheck(t, p, run, endpoint, key, "ST-SUP-RC32-OC06-T02") })
	t.Run("ST-SUP-RC32-OC07-T01 A committed, a denied and a failed command each leave one command audit row", func(t *testing.T) { runCheck(t, p, run, endpoint, key, "ST-SUP-RC32-OC07-T01") })
	t.Run("ST-SUP-RC32-OC08-T01 A retried command replays and does not call the fill again", func(t *testing.T) { runCheck(t, p, run, endpoint, key, "ST-SUP-RC32-OC08-T01") })
	t.Run("ST-SUP-RC32-OC08-T02 A command without an Idempotency-Key gets 400 and does not call the fill", func(t *testing.T) { runCheck(t, p, run, endpoint, key, "ST-SUP-RC32-OC08-T02") })
	t.Run("ST-SUP-RC32-OC09-T01 A command fill's error text reaches no response, audit row or completion log", func(t *testing.T) { runCheck(t, p, run, endpoint, key, "ST-SUP-RC32-OC09-T01") })
	t.Run("ST-SUP-RC32-OC10-T01 A command that loses a conflict gets 409 conflict, an audit row, and runs again on a retry with the same key", func(t *testing.T) { runCheck(t, p, run, endpoint, key, "ST-SUP-RC32-OC10-T01") })
	t.Run("ST-SUP-RC32-OC11-T01 HTTP and gRPC take one request message", func(t *testing.T) { runCheck(t, p, run, endpoint, key, "ST-SUP-RC32-OC11-T01") })
}
