package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	_ "github.com/jackc/pgx/v5/stdlib"
	"google.golang.org/grpc"

	"github.com/jsell-rh/stego/registry/runtime/audit"
	"github.com/jsell-rh/stego/registry/runtime/authz"
	"github.com/jsell-rh/stego/registry/runtime/internal/pgtest"
	"github.com/jsell-rh/stego/registry/runtime/outbox"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
)

// steps records the step names of Migrate and the backend of the connection that each runs on.
type steps struct {
	mu       sync.Mutex
	names    []string
	backends map[int]bool
}

func (s *steps) record(t *testing.T) func(string, *sql.Conn) {
	return func(name string, conn *sql.Conn) {
		var pid int
		if err := conn.QueryRowContext(context.Background(), "SELECT pg_backend_pid()").Scan(&pid); err != nil {
			t.Errorf("the step %s has no open connection: %v", name, err)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		s.names = append(s.names, name)
		s.backends[pid] = true
	}
}

func TestMigrate(t *testing.T) {
	t.Run("ST-SUP-RP14-LC03-T01 Migrate runs the migration job in its fixed order on one connection", func(t *testing.T) {
		endpoint, key := startSpiceDB(t)
		f := newFixture(t)
		settings := MigrationSettings{DatabaseURL: f.database, SpiceDB: SpiceDB{Endpoint: endpoint, Key: key, InsecureLoopback: true}}
		want := []string{StepOwnerRole, StepLedgerCheck, StepAuthorizationSchema, StepTables, StepOutboxQueue, StepAuditTables,
			StepIdempotencyKeys, StepAuthorizationTables, StepRuntimeGrants, StepAuditPartitions, StepSchemaAgain}
		run := func(m Migration) (*steps, error) {
			s := &steps{backends: map[int]bool{}}
			stepped = s.record(t)
			defer func() { stepped = nil }()
			return s, Migrate(context.Background(), settings, m)
		}
		writes := 0
		for _, pass := range []string{"a fresh database", "the same database again"} {
			s, err := run(f.migration(&writes))
			if err != nil {
				t.Fatalf("Migrate on %s: %v", pass, err)
			}
			if !slices.Equal(s.names, want) || len(s.backends) != 1 {
				t.Fatalf("Migrate on %s ran %v on %d connections, want %v on one", pass, s.names, len(s.backends), want)
			}
		}
		if writes != 4 {
			t.Fatalf("the schema was written %d times in two runs, want twice in each", writes)
		}
		// The job made the objects and the grants of the runtime role, and the partitions of the audit table.
		var granted bool
		if err := f.admin.QueryRow(`SELECT has_table_privilege($1, 'public.orders', 'INSERT') AND has_table_privilege($1, 'public.idempotency_keys', 'UPDATE')
 AND has_function_privilege($1, 'public.idempotency_keys_purge(timestamptz, integer)', 'EXECUTE') AND to_regclass('stego_outbox.messages') IS NOT NULL AND to_regclass('public.authz_floor') IS NOT NULL`, f.runtime).Scan(&granted); err != nil || !granted {
			t.Fatalf("the job did not make the objects and the grants of the runtime role: %v", err)
		}
		// The tables step made the tenant list of the tenant runtime, owned by the owner role, which the runtime role
		// reads and writes only through its functions (ST-SUP-RP04-LC07, ST-SUP-Q97).
		var listed bool
		if err := f.admin.QueryRow(`SELECT has_table_privilege($1, 'public.stego_tenants', 'SELECT') AND NOT has_table_privilege($1, 'public.stego_tenants', 'INSERT')
 AND has_function_privilege($1, 'public.stego_provision_tenant(text)', 'EXECUTE')
 AND (SELECT tableowner FROM pg_tables WHERE schemaname = 'public' AND tablename = 'stego_tenants') = $2`, f.runtime, f.owner).Scan(&listed); err != nil || !listed {
			t.Fatalf("the job did not make the tenant list for the runtime role: %v", err)
		}
		var partitions int
		if err := f.admin.QueryRow(`SELECT count(*) FROM pg_inherits WHERE inhparent = 'public.audit_event'::regclass`).Scan(&partitions); err != nil || partitions < 2 {
			t.Fatalf("the job made %d audit partitions, want this month and the next: %v", partitions, err)
		}

		// A step that fails stops the job there, with a fault that names it, and no later step runs.
		broken := f.migration(nil)
		broken.Authorization = "SELECT 1 / 0"
		s, err := run(broken)
		var step *StepError
		if !errors.As(err, &step) || step.Step != StepAuthorizationTables || Stage(err) != "migrate.authorization_tables" {
			t.Fatalf("the broken job gave %v, want a fault of the step %s", err, StepAuthorizationTables)
		}
		if last := s.names[len(s.names)-1]; last != StepAuthorizationTables || slices.Contains(s.names, StepRuntimeGrants) {
			t.Fatalf("the broken job ran %v, want it to stop at %s", s.names, StepAuthorizationTables)
		}
		// The read-only ledger check refuses a database whose tables this release did not make, before any write.
		if _, err := f.admin.Exec(`CREATE TABLE public.foreign_orders (id text)`); err != nil {
			t.Fatal(err)
		}
		writes = 0
		s, err = run(f.migration(&writes))
		if Stage(err) != "migrate.ledger_check" || writes != 0 || slices.Contains(s.names, StepTables) {
			t.Fatalf("the job on a foreign table gave %v after %v with %d schema writes, want a fault of the ledger check and no write", err, s.names, writes)
		}
		if Stage(errors.New("other")) != "migrate" || !strings.HasPrefix(err.Error(), "migrate: ledger_check: ") {
			t.Fatalf("Stage and the fault text are not the step's: %v", err)
		}
		// A job that lacks a part is refused before it opens the database.
		if err := Migrate(context.Background(), settings, Migration{}); Stage(err) != "migrate.settings" {
			t.Fatalf("a job with no parts gave %v", err)
		}
	})
}

func TestMain(m *testing.M) {
	code := pgtest.Main(m)
	stopSpiceDB()
	os.Exit(code)
}

const rolePassword = "service-test"

// fixture is one database of the local PostgreSQL server of pgtest with the roles of one service, as the tenancy
// component's role migration makes them: the owner role, the runtime role with a login, the worker and relay roles,
// and the owner's membership in the outbox relay role.
type fixture struct {
	t                             *testing.T
	admin                         *sql.DB
	database                      string // the superuser URL of the database
	owner, runtime, worker, relay string
	purge                         string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	suffix := make([]byte, 5)
	_, _ = rand.Read(suffix)
	tag := hex.EncodeToString(suffix)
	f := &fixture{t: t, owner: "svc_owner_" + tag, runtime: "svc_rt_" + tag, worker: "svc_worker_" + tag, relay: "svc_relay_" + tag,
		purge: "svc_" + tag + "_idempotency_purge"}
	root, err := sql.Open("pgx", pgtest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	f.database = pgtest.Database(t, "service")
	if f.admin, err = sql.Open("pgx", f.database); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.admin.Close() })
	name := strings.TrimPrefix(mustURL(t, f.database).Path, "/")
	for _, statement := range []string{
		"CREATE ROLE " + f.owner + " NOLOGIN",
		"CREATE ROLE " + f.runtime + " LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD '" + rolePassword + "'",
		"CREATE ROLE " + f.worker + " NOLOGIN NOINHERIT",
		"CREATE ROLE " + f.relay + " NOLOGIN NOINHERIT",
		"DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '" + outbox.RelayRole + "') THEN CREATE ROLE " + outbox.RelayRole + " NOLOGIN; END IF; END $$",
		"GRANT " + outbox.RelayRole + " TO " + f.owner + " WITH SET TRUE, INHERIT TRUE",
		"CREATE ROLE " + f.purge + " NOLOGIN NOINHERIT",
		"GRANT " + f.purge + " TO " + f.owner + " WITH SET TRUE, INHERIT FALSE",
		"GRANT CREATE ON DATABASE " + name + " TO " + f.owner,
	} {
		if _, err := root.ExecContext(context.Background(), statement); err != nil {
			t.Fatalf("%.60s: %v", statement, err)
		}
	}
	// As the tenancy role migration gives them: the owner may grant CREATE on public, which the idempotency
	// migration gives the purge role only while it makes the purge function.
	for _, statement := range []string{
		"GRANT CREATE ON SCHEMA public TO " + f.owner + " WITH GRANT OPTION",
		"GRANT CREATE ON SCHEMA public TO " + outbox.RelayRole,
	} {
		if _, err := f.admin.ExecContext(context.Background(), statement); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, role := range []string{f.runtime, f.worker, f.relay, f.owner, f.purge} {
			_, _ = f.admin.ExecContext(context.Background(), "DROP OWNED BY "+role)
		}
		_, _ = root.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		for _, role := range []string{f.runtime, f.worker, f.relay, f.owner, f.purge} {
			_, _ = root.ExecContext(context.Background(), "DROP ROLE IF EXISTS "+role)
		}
	})
	return f
}

func mustURL(t *testing.T, text string) *url.URL {
	t.Helper()
	u, err := url.Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// runtimeURL is the URL of the runtime role's login on the database.
func (f *fixture) runtimeURL() string {
	u := mustURL(f.t, f.database)
	u.User = url.UserPassword(f.runtime, rolePassword)
	return u.String()
}

// tenantConfig is the connection configuration of the adapter in the tests: the local server, without TLS on the
// loopback address, as the adapter's loopback test mode allows.
func tenantConfig(calls *int) func(string) (tenant.Config, error) {
	return func(dsn string) (tenant.Config, error) {
		if calls != nil {
			*calls++
		}
		return tenant.Config{DSN: dsn, InsecureLoopback: true}, nil
	}
}

// ordersDDL is the ledger of the test service: one tenant-owned table of orders with row-level security.
const ordersDDL = `CREATE TABLE IF NOT EXISTS public.orders (tenant_id text NOT NULL, id text NOT NULL, status text NOT NULL,
  version bigint NOT NULL DEFAULT 1, authz_token text NOT NULL DEFAULT '', PRIMARY KEY (tenant_id, id));
ALTER TABLE public.orders ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.orders FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS orders_tenant ON public.orders;
CREATE POLICY orders_tenant ON public.orders USING (tenant_id = current_setting('app.tenant', true))
  WITH CHECK (tenant_id = current_setting('app.tenant', true));`

// migration is the migration job of the test service, with a schema writer that writes into calls.
func (f *fixture) migration(calls *int) Migration {
	t := f.t
	queue, err := outbox.Migration(f.runtime, f.worker)
	if err != nil {
		t.Fatal(err)
	}
	auditText, err := audit.DDL(audit.Config{RuntimeRole: f.runtime, AnchorPeriod: time.Hour, RetentionMonths: 12})
	if err != nil {
		t.Fatal(err)
	}
	authzText, err := authz.Migration("public", f.runtime, f.relay, f.worker)
	if err != nil {
		t.Fatal(err)
	}
	return Migration{
		OwnerRole: f.owner, RuntimeRole: f.runtime, PurgeRole: f.purge, TenantConfig: tenantConfig(nil),
		LedgerCheck: func(ctx context.Context, conn *sql.Conn) error {
			var foreign bool
			if err := conn.QueryRowContext(ctx, `SELECT to_regclass('public.foreign_orders') IS NOT NULL`).Scan(&foreign); err != nil {
				return err
			}
			if foreign {
				return fmt.Errorf("the database holds a table that this release did not make")
			}
			return nil
		},
		Ledger: func(ctx context.Context, conn *sql.Conn) error {
			_, err := conn.ExecContext(ctx, ordersDDL)
			return err
		},
		Outbox: queue, Audit: auditText, Authorization: authzText,
		WriteSchema: func(ctx context.Context, conn grpc.ClientConnInterface) error {
			if calls != nil {
				*calls++
			}
			if spiceServer.endpoint == "" {
				return nil
			}
			_, err := v1.NewSchemaServiceClient(conn).WriteSchema(ctx, &v1.WriteSchemaRequest{Schema: spiceSchema})
			return err
		},
		Grants: []string{`GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE "public"."orders"`,
			`GRANT SELECT, INSERT, UPDATE ON TABLE "public"."idempotency_keys"`},
	}
}

// writeFile writes data to a new file of the test, which only its owner may read, and gives its path.
func writeFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// spiceDBImage is the pinned SpiceDB image of the local stand-in (ST-STD-LOCAL-R04), as the other runtime tests pin
// it.
const spiceDBImage = "authzed/spicedb:v1.57.0@sha256:a170d372c9f6b921bb6849064df6ca3566dd5b174882c1a2c4300c2ae1d60efd"

// spiceSchema is the schema of the test service.
const spiceSchema = `definition user {}
definition service {
	relation operator: user
	permission create_order = operator
}
definition order {
	relation owner: user
	permission view = owner
	permission create_order = owner
}
definition stego_floor {
	relation marker: stego_floor
}`

// spiceServer is the one SpiceDB container of the test binary, with its preshared key.
var spiceServer struct {
	once                sync.Once
	name, endpoint, key string
	err                 error
}

// startSpiceDB starts a local SpiceDB container without privilege, with an in-memory datastore and limits, once for
// the test binary, and gives its endpoint and preshared key.
func startSpiceDB(t *testing.T) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		if os.Getenv(pgtest.NoContainersVariable) == "1" {
			t.Skip("docker is not on the PATH, and STEGO_TEST_NO_CONTAINERS=1: this test needs a local SpiceDB container")
		}
		t.Fatal("docker is not on the PATH: this test needs a local SpiceDB container")
	}
	spiceServer.once.Do(func() {
		suffix := make([]byte, 6)
		_, _ = rand.Read(suffix)
		spiceServer.name, spiceServer.key = "stego-service-spicedb-"+hex.EncodeToString(suffix), "service-"+hex.EncodeToString(suffix)
		run := exec.Command("docker", "run", "-d", "--rm", "--name", spiceServer.name, "--memory", "256m", "--cpus", "1",
			"--cap-drop", "ALL", "--security-opt", "no-new-privileges", "-p", "127.0.0.1::50051", spiceDBImage,
			"serve", "--grpc-preshared-key", spiceServer.key, "--datastore-engine", "memory")
		var stderr bytes.Buffer
		run.Stderr = &stderr
		if err := run.Run(); err != nil {
			spiceServer.err = fmt.Errorf("start the SpiceDB container: %v: %s", err, stderr.String())
			return
		}
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
			out, err := exec.Command("docker", "port", spiceServer.name, "50051/tcp").Output()
			if err == nil && len(out) > 0 {
				spiceServer.endpoint = strings.TrimSpace(strings.Split(string(out), "\n")[0])
				break
			}
		}
		if spiceServer.endpoint == "" {
			spiceServer.err = fmt.Errorf("the SpiceDB container publishes no port")
			return
		}
		// The container answers once its health check serves.
		conn, err := DialSpiceDB(SpiceDB{Endpoint: spiceServer.endpoint, Key: spiceServer.key, InsecureLoopback: true})
		if err != nil {
			spiceServer.err = err
			return
		}
		defer conn.Close()
		for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
			if spiceServer.err = reach(context.Background(), conn); spiceServer.err == nil {
				return
			}
		}
	})
	if spiceServer.err != nil {
		t.Fatal(spiceServer.err)
	}
	return spiceServer.endpoint, spiceServer.key
}

func stopSpiceDB() {
	if spiceServer.name != "" {
		_ = exec.Command("docker", "rm", "-f", spiceServer.name).Run()
	}
}
