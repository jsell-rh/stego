package auditlog

// The fixture of the tests of the generated audit wiring. They run in the generated package, against the
// PostgreSQL server that STEGO_TEST_POSTGRES_DSN names, a superuser URL (ST-STD-LOCAL). Each test makes a
// database of its own, owned by the owner role, applies the emitted migration there as the owner, and drops the
// database at the end.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"

	"stego.dev/runtime/audit"
	"stego.dev/runtime/lease"
	"stego.dev/runtime/tenant"
)

const (
	ownerRole    = "stego_auditlog_owner"
	runtimeRole  = "stego_auditlog_runtime"
	rolePassword = "stego-auditlog-test"
	leaseName    = "controller"
)

var rolesOnce sync.Once

type fixture struct {
	t       *testing.T
	ctx     context.Context
	cfg     *pgx.ConnConfig
	db      string
	owner   *sql.DB
	runtime *sql.DB
	outbox  *recordingOutbox
	held    *lease.Held
}

// newFixture makes the database, applies the emitted migration as the owner, takes the controller lease, and
// creates the partitions with the emitted maintenance loop.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	dsn := os.Getenv("STEGO_TEST_POSTGRES_DSN")
	if dsn == "" {
		if os.Getenv("STEGO_REQUIRE_POSTGRES") == "1" {
			t.Fatal("the PostgreSQL tests need STEGO_TEST_POSTGRES_DSN")
		}
		t.Skip("set STEGO_TEST_POSTGRES_DSN to run the PostgreSQL tests")
	}
	ctx := context.Background()
	base, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin := stdlib.OpenDB(*base)
	t.Cleanup(func() { admin.Close() })
	rolesOnce.Do(func() {
		for _, role := range []string{ownerRole, runtimeRole} {
			_, err := admin.ExecContext(ctx, fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD '%s'", role, rolePassword))
			var pgErr *pgconn.PgError
			if err != nil && !(errors.As(err, &pgErr) && pgErr.Code == "42710") {
				t.Fatalf("create the role %s: %v", role, err)
			}
		}
	})
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, ctx: ctx, cfg: base, db: "auditlog_" + hex.EncodeToString(suffix[:]), outbox: &recordingOutbox{}}
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+f.db+" OWNER "+ownerRole); err != nil {
		t.Fatalf("create the database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.ExecContext(ctx, "DROP DATABASE "+f.db+" WITH (FORCE)"); err != nil {
			t.Errorf("drop the database %s: %v", f.db, err)
		}
	})
	f.owner = f.open(ownerRole)
	ddl, err := Migration(runtimeRole)
	if err != nil {
		t.Fatal(err)
	}
	setup := ddl + lease.LeasesTable + `;
INSERT INTO leases (name) VALUES ('` + leaseName + `');
GRANT SELECT, UPDATE ON leases TO ` + runtimeRole + `;`
	if _, err := f.owner.ExecContext(ctx, setup); err != nil {
		t.Fatalf("apply the emitted migration: %v", err)
	}
	f.runtime = f.open(runtimeRole)
	client, err := lease.New(f.runtime, leaseName, "holder-1", lease.Settings{})
	if err != nil {
		t.Fatal(err)
	}
	if f.held, err = client.Acquire(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.maintainer().Run(ctx); err != nil {
		t.Fatalf("the emitted maintenance loop: %v", err)
	}
	return f
}

func (f *fixture) open(role string) *sql.DB {
	cfg := f.cfg.Copy()
	cfg.Database, cfg.User, cfg.Password = f.db, role, rolePassword
	cfg.RuntimeParams = map[string]string{"TimeZone": "UTC"}
	db := stdlib.OpenDB(*cfg)
	f.t.Cleanup(func() { db.Close() })
	return db
}

func (f *fixture) leaseOf() *lease.Held { return f.held }

// jobs returns the emitted jobs of controller mode for the tenants ids.
func (f *fixture) jobs(ids ...string) (*audit.Anchorer, *audit.Maintainer) {
	f.t.Helper()
	list := make([]tenant.Tenant, len(ids))
	for i, id := range ids {
		list[i] = f.tn(id)
	}
	a, m, err := Jobs(f.leaseOf, func(context.Context) ([]tenant.Tenant, error) { return list, nil }, f.runtime, f.outbox)
	if err != nil {
		f.t.Fatal(err)
	}
	return a, m
}

func (f *fixture) maintainer() *audit.Maintainer {
	_, m := f.jobs()
	return m
}

// tn returns the tenant of the ID, built as the tenant runtime builds a tenant from a column that a statement read.
func (f *fixture) tn(id string) tenant.Tenant {
	f.t.Helper()
	rows, err := f.owner.QueryContext(f.ctx, "SELECT $1::text", id)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		f.t.Fatal("no row")
	}
	t, err := tenant.ScanRows(rows)
	if err != nil {
		f.t.Fatal(err)
	}
	return t
}

// inTx runs fn in a transaction of the tenant id on db, which tenant.Begin opens and tenant.Commit commits.
func (f *fixture) inTx(db *sql.DB, id string, fn func(tx *sql.Tx) error) error {
	t := f.tn(id)
	tx, err := tenant.Begin(f.ctx, db, t, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tenant.Commit(f.ctx, tx, t)
}

// change writes one audited change of a Customer of the tenant id through the audit writer, with the emitted
// settings and the emitted fields of Customer.
func (f *fixture) change(w *audit.Writer, id, resource string, before, after map[string]any) audit.Event {
	f.t.Helper()
	e := audit.Event{ActorID: "u1", ActorKind: audit.ActorUser, Action: "update", ResourceType: "Customer",
		ResourceID: resource, Outcome: audit.OutcomeOK, Service: "customers"}
	if err := e.SetStates(Settings, CustomerFields, before, after); err != nil {
		f.t.Fatal(err)
	}
	var stored []audit.Event
	err := f.inTx(f.runtime, id, func(tx *sql.Tx) error {
		var err error
		stored, err = w.Record(f.ctx, tx, f.tn(id), []audit.Event{e})
		return err
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return stored[0]
}

func (f *fixture) writer() *audit.Writer {
	f.t.Helper()
	w, err := audit.NewWriter(f.outbox, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	return w
}

// recordingOutbox is a stand-in for the outbox queue in the tests: it keeps each message it is given. The real
// queue is wired when the generated service pins the runtime (ST-SUP-Q64).
type recordingOutbox struct {
	mu       sync.Mutex
	messages []message
}

type message struct {
	tenant, destination, key string
	payload                  []byte
}

func (o *recordingOutbox) Enqueue(_ context.Context, _ audit.Tx, tenant, destination, key string, payload []byte) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.messages = append(o.messages, message{tenant, destination, key, append([]byte(nil), payload...)})
	return nil
}

func (o *recordingOutbox) all() []message {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]message(nil), o.messages...)
}
