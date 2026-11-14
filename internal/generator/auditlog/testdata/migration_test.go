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
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/jsell-rh/stego/registry/runtime/audit"
	"github.com/jsell-rh/stego/registry/runtime/lease"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
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
	// The drop gets 30 seconds of its own, not what is left of the test's context, and a drop that fails is logged:
	// the cleanup alone never fails the test.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := admin.ExecContext(ctx, "DROP DATABASE "+f.db+" WITH (FORCE)"); err != nil {
			t.Logf("pgtest: cleanup failed: drop the database %s: %v", f.db, err)
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
// queue is wired through the adapter of the audit runtime (ST-SUP-RP05-LC14) when it is on main.
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

// rejected runs the statement q in a transaction of the tenant t1 on db, and returns its error text.
func (f *fixture) rejected(db *sql.DB, q string) string {
	f.t.Helper()
	err := f.inTx(db, "t1", func(tx *sql.Tx) error {
		_, err := tx.ExecContext(f.ctx, q)
		return err
	})
	if err == nil {
		f.t.Fatalf("%s succeeded", q)
	}
	return err.Error()
}

func (f *fixture) rows(id string) int64 {
	f.t.Helper()
	var n int64
	if err := f.inTx(f.owner, id, func(tx *sql.Tx) error {
		return tx.QueryRowContext(f.ctx, "SELECT count(*) FROM audit_event").Scan(&n)
	}); err != nil {
		f.t.Fatal(err)
	}
	return n
}

var monthBound = regexp.MustCompile(`^FOR VALUES FROM \('(\d{4})-(\d{2})-01 00:00:00\+00'\) TO \('(\d{4})-(\d{2})-01 00:00:00\+00'\)$`)

func TestMigration(t *testing.T) {
	t.Run("ST-SUP-RC28-OC06-T01 The migration creates one audit_event table partitioned by month", func(t *testing.T) {
		f := newFixture(t)
		// The service declares three entities; there is still one audit table.
		var tables int
		if err := f.owner.QueryRowContext(f.ctx, `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p') AND NOT c.relispartition
   AND c.relname NOT IN ('audit_chain_heads', 'audit_anchor_boundaries', 'audit_retention_checkpoints', 'leases')`).Scan(&tables); err != nil {
			t.Fatal(err)
		}
		if tables != 1 {
			t.Fatalf("%d audit tables that are not partitions, want exactly one", tables)
		}
		var strategy, column string
		if err := f.owner.QueryRowContext(f.ctx, `SELECT p.partstrat::text, a.attname FROM pg_partitioned_table p
 JOIN pg_attribute a ON a.attrelid = p.partrelid AND a.attnum = p.partattrs[0]
 WHERE p.partrelid = 'public.audit_event'::regclass`).Scan(&strategy, &column); err != nil {
			t.Fatalf("audit_event is not a partitioned table: %v", err)
		}
		if strategy != "r" || column != "recorded_at" {
			t.Fatalf("audit_event is partitioned by %s on %s, want RANGE on recorded_at", strategy, column)
		}
		rows, err := f.owner.QueryContext(f.ctx, `SELECT pg_get_expr(c.relpartbound, c.oid) FROM pg_inherits i
 JOIN pg_class c ON c.oid = i.inhrelid WHERE i.inhparent = 'public.audit_event'::regclass`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		partitions := 0
		for rows.Next() {
			var bound string
			if err := rows.Scan(&bound); err != nil {
				t.Fatal(err)
			}
			m := monthBound.FindStringSubmatch(bound)
			if m == nil {
				t.Fatalf("the partition bound %s is not one month", bound)
			}
			from, _ := time.Parse("2006-01", m[1]+"-"+m[2])
			to, _ := time.Parse("2006-01", m[3]+"-"+m[4])
			if !from.AddDate(0, 1, 0).Equal(to) {
				t.Fatalf("the partition bound %s is not one month", bound)
			}
			partitions++
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if partitions < 4 {
			t.Fatalf("%d partitions, want the current month and the next three", partitions)
		}
	})

	t.Run("ST-SUP-RC28-OC06-T02 The runtime role can select from and insert into audit_event, and cannot update, delete or truncate", func(t *testing.T) {
		f := newFixture(t)
		f.change(f.writer(), "t1", "c1", nil, map[string]any{"plan": "free"})
		err := f.inTx(f.runtime, "t1", func(tx *sql.Tx) error {
			var n int
			if err := tx.QueryRowContext(f.ctx, "SELECT count(*) FROM audit_event").Scan(&n); err != nil {
				return err
			}
			_, err := tx.ExecContext(f.ctx, `INSERT INTO audit_event (tenant_id, seq, id, occurred_at, recorded_at, actor_id,
 actor_kind, actor_ip, user_agent, action, resource_type, resource_id, outcome, error_code, request_id, trace_id,
 span_id, idempotency_key, service, service_version, declaration_version, prev_hash, hash)
 VALUES ('t1', 99, gen_random_uuid(), now(), now(), 'u1', 'user', '', '', 'create', 'Customer', 'c9', 'ok', '', '',
 '', '', '', 'customers', '', '', '\x00', '\x00')`)
			return err
		})
		if err != nil {
			t.Fatalf("SELECT and INSERT as the runtime role: %v", err)
		}
		for _, q := range []string{"UPDATE audit_event SET action = 'x'", "DELETE FROM audit_event", "TRUNCATE audit_event"} {
			if text := f.rejected(f.runtime, q); !strings.Contains(text, "permission denied") {
				t.Errorf("%s as the runtime role: %s, want a permission error", q, text)
			}
		}
	})

	t.Run("ST-SUP-RC28-OC06-T03 The reject trigger refuses UPDATE, DELETE and TRUNCATE also for the owner role", func(t *testing.T) {
		f := newFixture(t)
		f.change(f.writer(), "t1", "c1", nil, map[string]any{"plan": "free"})
		before := f.rows("t1")
		for _, q := range []string{"UPDATE audit_event SET action = 'x'", "DELETE FROM audit_event", "TRUNCATE audit_event"} {
			if text := f.rejected(f.owner, q); !strings.Contains(text, "the audit table rejects") {
				t.Errorf("%s as the owner role: %s, want the error of the trigger", q, text)
			}
		}
		var action string
		if err := f.inTx(f.owner, "t1", func(tx *sql.Tx) error {
			return tx.QueryRowContext(f.ctx, "SELECT action FROM audit_event WHERE resource_id = 'c1'").Scan(&action)
		}); err != nil {
			t.Fatal(err)
		}
		if after := f.rows("t1"); after != before || action != "update" {
			t.Fatalf("rows %d then %d, action %q: the rows changed", before, after, action)
		}
	})

	t.Run("ST-SUP-RC28-OC06-T04 The emitted audit tables keep each tenant's rows apart", func(t *testing.T) {
		f := newFixture(t)
		w := f.writer()
		f.change(w, "t1", "c1", nil, map[string]any{"plan": "free"})
		f.change(w, "t2", "c2", nil, map[string]any{"plan": "gold"})
		f.change(w, "t1", "c3", nil, map[string]any{"plan": "gold"})
		var tenants []string
		err := f.inTx(f.runtime, "t1", func(tx *sql.Tx) error {
			for _, table := range []string{"audit_event", "audit_chain_heads"} {
				rows, err := tx.QueryContext(f.ctx, "SELECT tenant_id FROM "+table)
				if err != nil {
					return err
				}
				for rows.Next() {
					var id string
					if err := rows.Scan(&id); err != nil {
						rows.Close()
						return err
					}
					tenants = append(tenants, table+":"+id)
				}
				rows.Close()
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		want := "audit_event:t1 audit_event:t1 audit_chain_heads:t1"
		if strings.Join(tenants, " ") != want {
			t.Fatalf("the rows of t1's transaction are %v, want %s", tenants, want)
		}
	})
}
