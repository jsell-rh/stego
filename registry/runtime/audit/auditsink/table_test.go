package auditsink

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/jsell-rh/stego/registry/runtime/audit"
	"github.com/jsell-rh/stego/registry/runtime/audit/auditoutbox"
	"github.com/jsell-rh/stego/registry/runtime/internal/pgtest"
	"github.com/jsell-rh/stego/registry/runtime/internal/seal"
	"github.com/jsell-rh/stego/registry/runtime/outbox"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

const rolePassword = "auditsink-test"

// env is one database of the local PostgreSQL server of pgtest (ST-STD-LOCAL) with the outbox runtime's queue, the
// audit runtime's tables and the default sink, which an owner role made, and a runtime role and a worker role that
// are not superusers.
type env struct {
	admin                    *sql.DB
	runtime, worker          *sql.DB
	ownerRole, workerRole    string
	runtimeRole              string
	queue                    *auditoutbox.Queue
	writer                   *audit.Writer
	runtimeQueue, workerSide *outbox.Queue
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	root, err := sql.Open("pgx", pgtest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	tag := hex.EncodeToString(suffix)
	roles := [3]string{"as_owner_" + tag, "as_rt_" + tag, "as_worker_" + tag}
	for _, role := range roles {
		if _, err := root.ExecContext(ctx, "CREATE ROLE "+role+" LOGIN NOSUPERUSER NOBYPASSRLS NOINHERIT PASSWORD '"+rolePassword+"'"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = root.ExecContext(context.Background(), "DROP ROLE IF EXISTS "+role) })
	}
	database := pgtest.Database(t, "auditsink")
	admin, err := sql.Open("pgx", database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	t.Cleanup(func() {
		for _, role := range roles {
			_, _ = admin.ExecContext(context.Background(), "DROP OWNED BY "+role)
		}
	})
	u, err := url.Parse(database)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(ctx, "GRANT CREATE ON DATABASE "+u.Path[1:]+" TO "+roles[0]); err != nil {
		t.Fatal(err)
	}
	queueDDL, err := outbox.Migration(roles[1], roles[2])
	if err != nil {
		t.Fatal(err)
	}
	auditDDL, err := audit.DDL(audit.Config{RuntimeRole: roles[1], AnchorPeriod: time.Hour, RetentionMonths: 12})
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{queueDDL, auditDDL} {
		if _, err := admin.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%.80s: %v", statement, err)
		}
	}
	for more := true; more; {
		if err := admin.QueryRowContext(ctx, "SELECT bool_or(step_left) FROM "+audit.MaintainFunction+"()").Scan(&more); err != nil {
			t.Fatalf("audit_maintain: %v", err)
		}
	}
	e := &env{admin: admin, ownerRole: roles[0], runtimeRole: roles[1], workerRole: roles[2]}
	e.migrate(t)
	open := func(role string) *sql.DB {
		v := *u
		v.User = url.UserPassword(role, rolePassword)
		db, err := sql.Open("pgx", v.String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	e.runtime, e.worker = open(roles[1]), open(roles[2])
	if e.runtimeQueue, err = outbox.New(e.runtime, outbox.Options{PayloadLimits: auditoutbox.PayloadLimits()}); err != nil {
		t.Fatal(err)
	}
	if e.queue, err = auditoutbox.NewQueue(e.runtimeQueue); err != nil {
		t.Fatal(err)
	}
	if e.writer, err = audit.NewWriter(e.queue, nil); err != nil {
		t.Fatal(err)
	}
	if e.workerSide, err = outbox.New(e.worker, outbox.Options{}); err != nil {
		t.Fatal(err)
	}
	return e
}

// migrate runs the migration of the default sink as the owner role, in one transaction, as the migration job does.
func (e *env) migrate(t *testing.T) {
	t.Helper()
	text, err := Migration(e.workerRole)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := e.admin.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec("SET LOCAL ROLE " + e.ownerRole); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(text); err != nil {
		t.Fatalf("the migration of the default sink: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func (e *env) count(t *testing.T, statement string, args ...any) int {
	t.Helper()
	var n int
	if err := e.admin.QueryRowContext(context.Background(), statement, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

type decision struct{ seal.DecisionMark }

func mustTenant(t *testing.T, id string) tenant.Tenant {
	t.Helper()
	tn, err := tenant.New(tenant.Requested(tenant.ProvisionAction, id, decision{seal.NewDecisionMark(id, "provision")}))
	if err != nil {
		t.Fatal(err)
	}
	return tn
}

// record writes one audit event of the tenant through the Writer and enqueues the anchor a for the sink, in one
// committed transaction, and gives the stored event.
func (e *env) record(t *testing.T, tenantID, requestID string, a *audit.Anchor) audit.Event {
	t.Helper()
	ctx := context.Background()
	tn := mustTenant(t, tenantID)
	tx, err := tenant.Begin(ctx, e.runtime, tn, nil)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := e.writer.Record(ctx, tx, tn, []audit.Event{{TenantID: tenantID, ActorID: "user:u-7",
		ActorKind: audit.ActorUser, Action: "create", ResourceType: "order", ResourceID: "o-" + requestID,
		Outcome: audit.OutcomeOK, RequestID: requestID, Service: "orders"}})
	if err != nil {
		t.Fatal(err)
	}
	if a != nil {
		if err := e.queue.Enqueue(ctx, tx, tenantID, audit.SinkDestination, "anchor:"+tenantID+":"+requestID, audit.AnchorMessage(*a)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tenant.Commit(ctx, tx, tn); err != nil {
		t.Fatal(err)
	}
	return stored[0]
}

// worker starts the outbox worker as the worker role with the audit sink destination on sink, and gives the function
// that stops it.
func (e *env) startWorker(t *testing.T, sink audit.Sink) func() {
	t.Helper()
	destination, err := audit.NewDestination(sink)
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := auditoutbox.NewSink(destination)
	if err != nil {
		t.Fatal(err)
	}
	config := outbox.DefaultWorkerConfig()
	config.PollInterval, config.RetryMin, config.RetryMax = 20*time.Millisecond, 20*time.Millisecond, 100*time.Millisecond
	worker, err := outbox.NewWorker(e.workerSide, config)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Register(adapter.Destination()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	return func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("the worker ended with %v", err)
		}
	}
}

// waitFor waits up to ten seconds until cond holds.
func waitFor(cond func() bool) bool {
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return true
		}
	}
	return cond()
}

func TestTableSink(t *testing.T) {
	t.Run("ST-SUP-RP05-LC15-T01 Entries and anchors land in the default table once, and the worker role cannot change them", func(t *testing.T) {
		e := newEnv(t)
		ctx := context.Background()
		anchor := audit.Anchor{Tenant: "t1", Kind: audit.AnchorPeriodic, Head: bytes.Repeat([]byte{7}, 32), Count: 1}
		event := e.record(t, "t1", "r1", &anchor)
		sink, err := NewTableSink(e.worker)
		if err != nil {
			t.Fatal(err)
		}
		stop := e.startWorker(t, sink)
		if !waitFor(func() bool { return e.count(t, "SELECT count(*) FROM stego_outbox.messages WHERE state = 'done'") == 2 }) {
			stop()
			t.Fatalf("the worker did not deliver the event and the anchor to the default sink")
		}
		stop()

		// The event row holds the event's ID, hash, previous hash, tenant and its canonical sink message, the bytes of
		// its outbox row.
		var kind, tenantID string
		var hash, prev, body, payload []byte
		if err := e.admin.QueryRowContext(ctx, "SELECT kind, tenant_id, hash, prev_hash, body FROM "+Schema+"."+Table+" WHERE entry_id = $1",
			event.ID.String()).Scan(&kind, &tenantID, &hash, &prev, &body); err != nil {
			t.Fatalf("the default table holds no row of the event: %v", err)
		}
		if err := e.admin.QueryRowContext(ctx, "SELECT payload FROM stego_outbox.messages WHERE id = $1::uuid", event.ID.String()).Scan(&payload); err != nil {
			t.Fatal(err)
		}
		if kind != KindEvent || tenantID != "t1" || !bytes.Equal(hash, event.Hash) || !bytes.Equal(prev, event.PrevHash) || !bytes.Equal(body, payload) {
			t.Fatalf("the event row is %s %s with hash %x, prev %x and body\n%s\nwant event t1, the event's hashes and the outbox payload\n%s",
				kind, tenantID, hash, prev, body, payload)
		}
		// The anchor row holds the head hash, no previous hash and the canonical anchor message.
		anchorEntry, err := anchorEntry(anchor)
		if err != nil {
			t.Fatal(err)
		}
		var anchorPrev []byte
		if err := e.admin.QueryRowContext(ctx, "SELECT kind, tenant_id, hash, prev_hash, body FROM "+Schema+"."+Table+" WHERE entry_id = $1",
			anchorEntry.ID).Scan(&kind, &tenantID, &hash, &anchorPrev, &body); err != nil {
			t.Fatalf("the default table holds no row of the anchor: %v", err)
		}
		if kind != KindAnchor || tenantID != "t1" || !bytes.Equal(hash, anchor.Head) || anchorPrev != nil || !bytes.Equal(body, audit.AnchorMessage(anchor)) {
			t.Fatalf("the anchor row is %s %s with hash %x, prev %x and body %s", kind, tenantID, hash, anchorPrev, body)
		}

		// A second send of each entry stores nothing new: the event is a duplicate, which the destination counts as
		// a success, and the anchor is a success.
		before := e.count(t, "SELECT count(*) FROM "+Schema+"."+Table)
		if err := sink.Append(ctx, []audit.Event{event}); !errors.Is(err, audit.ErrDuplicate) {
			t.Fatalf("a second Append of the event gave %v, want an error that wraps audit.ErrDuplicate", err)
		}
		if err := sink.Anchor(ctx, anchor); err != nil {
			t.Fatalf("a second Anchor gave %v, want nil", err)
		}
		destination, err := audit.NewDestination(sink)
		if err != nil {
			t.Fatal(err)
		}
		if err := destination.DeliverFor(ctx, "t1", payload); err != nil {
			t.Fatalf("a second delivery of the event message gave %v, want a success", err)
		}
		if after := e.count(t, "SELECT count(*) FROM "+Schema+"."+Table); after != before || before != 2 {
			t.Fatalf("the default table held %d rows and holds %d after a second send, want 2 and 2", before, after)
		}

		// The worker role reads and inserts, and cannot update, delete or truncate; the public role has nothing.
		for _, statement := range []string{
			"UPDATE " + Schema + "." + Table + " SET body = 'x'",
			"DELETE FROM " + Schema + "." + Table,
			"TRUNCATE " + Schema + "." + Table,
		} {
			_, err := e.worker.ExecContext(ctx, statement)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
				t.Errorf("the worker role ran %q: %v, want permission denied (42501)", strings.Fields(statement)[0], err)
			}
		}
		if n := e.count(t, "SELECT count(*) FROM "+Schema+"."+Table); n != 2 {
			t.Fatalf("the default table holds %d rows after the refused changes, want 2", n)
		}
		var owner string
		var publicUse bool
		if err := e.admin.QueryRowContext(ctx, `SELECT tableowner, has_schema_privilege('public', $1, 'USAGE') FROM pg_tables WHERE schemaname = $1 AND tablename = $2`,
			Schema, Table).Scan(&owner, &publicUse); err != nil {
			t.Fatal(err)
		}
		if owner != e.ownerRole || publicUse {
			t.Fatalf("the default table is owned by %s and the public role has USAGE %t, want %s and false", owner, publicUse, e.ownerRole)
		}
		// By the catalog: the runtime role of the api mode has no privilege on the table, and the worker role has no
		// privilege but SELECT and INSERT.
		for _, c := range []struct {
			role, privilege string
			want            bool
		}{
			{e.runtimeRole, "SELECT", false}, {e.runtimeRole, "INSERT", false}, {e.runtimeRole, "UPDATE", false}, {e.runtimeRole, "DELETE", false},
			{e.workerRole, "SELECT", true}, {e.workerRole, "INSERT", true}, {e.workerRole, "UPDATE", false}, {e.workerRole, "DELETE", false},
			{e.workerRole, "TRUNCATE", false}, {e.workerRole, "TRIGGER", false}, {e.workerRole, "REFERENCES", false},
		} {
			var has bool
			if err := e.admin.QueryRowContext(ctx, `SELECT has_table_privilege($1, 'stego_audit_sink.entries', $2)`, c.role, c.privilege).Scan(&has); err != nil {
				t.Fatal(err)
			}
			if has != c.want {
				t.Errorf("the role %s has %s on the default table: %t, want %t", c.role, c.privilege, has, c.want)
			}
		}
		// The migration runs again on a database that it made.
		e.migrate(t)
	})

	t.Run("ST-SUP-RP05-LC15-T02 Nothing leaves the outgoing queue until the default table takes it", func(t *testing.T) {
		e := newEnv(t)
		ctx := context.Background()
		// The worker role has lost INSERT, so the table refuses the entry.
		if _, err := e.admin.ExecContext(ctx, "REVOKE INSERT ON "+Schema+"."+Table+" FROM "+e.workerRole); err != nil {
			t.Fatal(err)
		}
		event := e.record(t, "t1", "r1", nil)
		sink, err := NewTableSink(e.worker)
		if err != nil {
			t.Fatal(err)
		}
		stop := e.startWorker(t, sink)
		attempted := waitFor(func() bool {
			return e.count(t, "SELECT count(*) FROM stego_outbox.messages WHERE attempts >= 2 AND state <> 'done'") == 1
		})
		if !attempted || e.count(t, "SELECT count(*) FROM "+Schema+"."+Table) != 0 {
			stop()
			t.Fatalf("the refused entry was not retried and kept in the outbox")
		}
		if _, err := e.admin.ExecContext(ctx, "GRANT INSERT ON "+Schema+"."+Table+" TO "+e.workerRole); err != nil {
			t.Fatal(err)
		}
		landed := waitFor(func() bool { return e.count(t, "SELECT count(*) FROM stego_outbox.messages WHERE state = 'done'") == 1 })
		stop()
		if !landed || e.count(t, "SELECT count(*) FROM "+Schema+"."+Table+" WHERE entry_id = $1", event.ID.String()) != 1 {
			t.Fatalf("the entry did not land once the table took it")
		}
	})
}

func TestTableConflict(t *testing.T) {
	t.Run("ST-SUP-RP05-LC15-T04 A row of another entry under a queued ID keeps the message in the outbox", func(t *testing.T) {
		e := newEnv(t)
		ctx := context.Background()
		event := e.record(t, "t1", "r1", nil)
		// The attack of the security review of PR #204: the worker role writes a row under the ID of a queued event
		// before the worker delivers it.
		if _, err := e.worker.ExecContext(ctx, "INSERT INTO "+Schema+"."+Table+" (entry_id, kind, tenant_id, hash, body) VALUES ($1, 'event', 't1', $2, 'forged')",
			event.ID.String(), event.Hash); err != nil {
			t.Fatal(err)
		}
		sink, err := NewTableSink(e.worker)
		if err != nil {
			t.Fatal(err)
		}
		if err := sink.Append(ctx, []audit.Event{event}); !errors.Is(err, ErrConflict) || errors.Is(err, audit.ErrDuplicate) {
			t.Fatalf("Append over another entry gave %v, want ErrConflict and no audit.ErrDuplicate", err)
		}
		stop := e.startWorker(t, sink)
		kept := waitFor(func() bool {
			return e.count(t, "SELECT count(*) FROM stego_outbox.messages WHERE id = $1::uuid AND attempts >= 2 AND state <> 'done'", event.ID.String()) == 1
		})
		stop()
		if !kept {
			t.Fatalf("the message of the event left the outbox over a row of another entry")
		}
		var body string
		if err := e.admin.QueryRowContext(ctx, "SELECT convert_from(body, 'UTF8') FROM "+Schema+"."+Table+" WHERE entry_id = $1", event.ID.String()).Scan(&body); err != nil || body != "forged" {
			t.Fatalf("the row under the ID is %q, %v, want the forged row left as it is", body, err)
		}
	})
}

func TestTableMigration(t *testing.T) {
	t.Run("ST-SUP-RP05-LC15-T03 The migration of the default table refuses a role name that is not a SQL name", func(t *testing.T) {
		for _, role := range []string{"", "Shop", `shop"; DROP TABLE x; --`, strings.Repeat("a", 64)} {
			if _, err := Migration(role); err == nil {
				t.Errorf("Migration(%q) gave no error", role)
			}
		}
		text, err := Migration("shop_worker")
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{`GRANT SELECT, INSERT ON TABLE stego_audit_sink.entries TO "shop_worker"`, "REVOKE ALL ON TABLE stego_audit_sink.entries FROM PUBLIC"} {
			if !strings.Contains(text, want) {
				t.Errorf("the migration does not hold %s", want)
			}
		}
		if strings.Contains(text, "UPDATE") || strings.Contains(text, "DELETE") || strings.Contains(text, "TRUNCATE") {
			t.Errorf("the migration grants a change:\n%s", text)
		}
	})
}
