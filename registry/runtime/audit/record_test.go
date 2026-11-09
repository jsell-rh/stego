package audit

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The PostgreSQL tests read STEGO_TEST_POSTGRES_DSN, the address of a superuser of a local PostgreSQL 17 server
// in a container without privilege (ST-STD-LOCAL), and skip when it is not set. Each test makes a database of its
// own, owned by the owner role, applies the DDL text there, and drops the database at the end.

const (
	ownerRole      = "stego_audit_owner"
	runtimeRole    = "stego_audit_runtime"
	rolePassword   = "stego-audit-test"
	outboxStandIn  = "test_outbox"
	appTable       = "app_items"
	rejectMessage  = "the audit table rejects"
	testRetention  = 12
	postgresDSNEnv = "STEGO_TEST_POSTGRES_DSN"
)

var rolesOnce sync.Once

type pgFixture struct {
	t       *testing.T
	ctx     context.Context
	owner   *pgx.Conn
	runtime *pgx.Conn
	other   *pgx.Conn
	cfg     *pgx.ConnConfig
}

func newPG(t *testing.T) *pgFixture {
	t.Helper()
	dsn := os.Getenv(postgresDSNEnv)
	if dsn == "" {
		t.Skip(postgresDSNEnv + " is not set: this test needs a PostgreSQL server")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect as the superuser: %v", err)
	}
	t.Cleanup(func() { admin.Close(ctx) })
	rolesOnce.Do(func() {
		for _, role := range []string{ownerRole, runtimeRole} {
			_, err := admin.Exec(ctx, fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD '%s'", role, rolePassword))
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
	db := "audit_" + hex.EncodeToString(suffix[:])
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+db+" OWNER "+ownerRole); err != nil {
		t.Fatalf("create the database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(ctx, "DROP DATABASE "+db+" WITH (FORCE)"); err != nil {
			t.Errorf("drop the database %s: %v", db, err)
		}
	})
	base, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	f := &pgFixture{t: t, ctx: ctx, cfg: base}
	f.owner = f.connect(db, ownerRole)
	ddl, err := DDL(runtimeRole)
	if err != nil {
		t.Fatal(err)
	}
	setup := ddl + `
CREATE TABLE ` + outboxStandIn + ` (n bigserial PRIMARY KEY, destination text NOT NULL, key text NOT NULL, payload bytea NOT NULL);
GRANT SELECT, INSERT ON ` + outboxStandIn + ` TO ` + runtimeRole + `;
GRANT USAGE ON SEQUENCE ` + outboxStandIn + `_n_seq TO ` + runtimeRole + `;
CREATE TABLE ` + appTable + ` (id text PRIMARY KEY, email text);
GRANT SELECT, INSERT ON ` + appTable + ` TO ` + runtimeRole + `;`
	if _, err := f.owner.Exec(ctx, setup); err != nil {
		t.Fatalf("apply the DDL text: %v", err)
	}
	f.inOwnerTx(func(tx pgx.Tx) error { return Maintain(ctx, tx, time.Now(), testRetention) })
	f.runtime = f.connect(db, runtimeRole)
	f.other = f.connect(db, runtimeRole)
	return f
}

func (f *pgFixture) connect(db, role string) *pgx.Conn {
	f.t.Helper()
	cfg := f.cfg.Copy()
	cfg.Database, cfg.User, cfg.Password = db, role, rolePassword
	cfg.RuntimeParams = map[string]string{"TimeZone": "UTC"}
	c, err := pgx.ConnectConfig(f.ctx, cfg)
	if err != nil {
		f.t.Fatalf("connect as %s: %v", role, err)
	}
	f.t.Cleanup(func() { c.Close(f.ctx) })
	return c
}

func (f *pgFixture) inOwnerTx(fn func(tx pgx.Tx) error) {
	f.t.Helper()
	if err := pgx.BeginFunc(f.ctx, f.owner, fn); err != nil {
		f.t.Fatalf("owner transaction: %v", err)
	}
}

// runTx opens short transactions on the runtime connection, as the service's transaction helper does, and
// records the statements of each.
type txLog struct {
	mu  sync.Mutex
	txs [][]string
}

func (f *pgFixture) runTx(log *txLog) RunTx {
	return func(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error {
		return pgx.BeginFunc(ctx, f.runtime, func(tx pgx.Tx) error {
			rec := &recordingTx{Tx: tx}
			err := fn(ctx, rec)
			if log != nil {
				log.mu.Lock()
				log.txs = append(log.txs, rec.statements)
				log.mu.Unlock()
			}
			return err
		})
	}
}

type recordingTx struct {
	pgx.Tx
	statements []string
}

func (r *recordingTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	r.statements = append(r.statements, sql)
	return r.Tx.Exec(ctx, sql, args...)
}

func (r *recordingTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	r.statements = append(r.statements, sql)
	return r.Tx.QueryRow(ctx, sql, args...)
}

func (r *recordingTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	r.statements = append(r.statements, sql)
	return r.Tx.Query(ctx, sql, args...)
}

// standInOutbox is the outbox stand-in: it writes the message into a table through the caller's transaction, as
// the outbox runtime's queue does, until that runtime is on main.
type standInOutbox struct {
	mu   sync.Mutex
	fail bool
}

func (o *standInOutbox) Enqueue(ctx context.Context, tx Tx, destination, key string, payload []byte) error {
	o.mu.Lock()
	fail := o.fail
	o.mu.Unlock()
	if fail {
		return errors.New("the outbox refuses the message")
	}
	_, err := tx.Exec(ctx, "INSERT INTO "+outboxStandIn+" (destination, key, payload) VALUES ($1, $2, $3)",
		destination, key, payload)
	return err
}

func (o *standInOutbox) setFail(v bool) {
	o.mu.Lock()
	o.fail = v
	o.mu.Unlock()
}

type outboxMessage struct {
	destination, key string
	payload          []byte
}

func (f *pgFixture) messages() []outboxMessage {
	f.t.Helper()
	rows, err := f.other.Query(f.ctx, "SELECT destination, key, payload FROM "+outboxStandIn+" ORDER BY n")
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []outboxMessage
	for rows.Next() {
		var m outboxMessage
		if err := rows.Scan(&m.destination, &m.key, &m.payload); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func (f *pgFixture) count(sql string, args ...any) int64 {
	f.t.Helper()
	var n int64
	if err := f.other.QueryRow(f.ctx, sql, args...).Scan(&n); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func (f *pgFixture) auditRows() int64 {
	return f.count("SELECT count(*) FROM " + EventsTable)
}

func (f *pgFixture) writer(outbox Enqueuer) *Writer {
	f.t.Helper()
	w, err := NewWriter(outbox, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	return w
}

func (f *pgFixture) begin() pgx.Tx {
	f.t.Helper()
	tx, err := f.runtime.Begin(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	return tx
}

func sampleEvent(service, action string) Event {
	return Event{
		TenantID:     "t-1",
		ActorID:      "user:u-7",
		ActorKind:    ActorUser,
		ActorIP:      "203.0.113.9",
		UserAgent:    "test-agent/1.0",
		Action:       action,
		ResourceType: "item",
		ResourceID:   "i-1",
		Outcome:      OutcomeOK,
		RequestID:    "r-1",
		Service:      service,
	}
}

// recordN writes n committed rows for the service.
func (f *pgFixture) recordN(w *Writer, service string, n int) []Event {
	f.t.Helper()
	var out []Event
	for i := 0; i < n; i++ {
		tx := f.begin()
		e := sampleEvent(service, fmt.Sprintf("update-%d", i+1))
		e.ResourceID = fmt.Sprintf("i-%d", i+1)
		stored, err := w.Record(f.ctx, tx, e)
		if err != nil {
			f.t.Fatalf("Record: %v", err)
		}
		if err := tx.Commit(f.ctx); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, stored)
	}
	return out
}

func auditStatement(sql string) bool {
	return strings.Contains(sql, EventsTable) || strings.Contains(sql, HeadsTable) || strings.Contains(sql, outboxStandIn)
}

func TestRecord(t *testing.T) {
	t.Run("ST-SUP-RP05-LC02-T01 An audit entry is visible only after the caller commits", func(t *testing.T) {
		f := newPG(t)
		w := f.writer(&standInOutbox{})
		tx := f.begin()
		if _, err := tx.Exec(f.ctx, "INSERT INTO "+appTable+" (id) VALUES ('i-1')"); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Record(f.ctx, tx, sampleEvent("alpha", "create")); err != nil {
			t.Fatal(err)
		}
		if n := f.auditRows(); n != 0 {
			t.Fatalf("a second session sees %d audit rows before the commit, want 0", n)
		}
		if err := tx.Commit(f.ctx); err != nil {
			t.Fatal(err)
		}
		if n := f.auditRows(); n != 1 {
			t.Fatalf("a second session sees %d audit rows after the commit, want 1", n)
		}
	})

	t.Run("ST-SUP-RP05-LC02-T02 A rolled-back caller transaction leaves no audit row", func(t *testing.T) {
		f := newPG(t)
		w := f.writer(&standInOutbox{})
		tx := f.begin()
		if _, err := w.Record(f.ctx, tx, sampleEvent("alpha", "create")); err != nil {
			t.Fatal(err)
		}
		if s := f.runtime.PgConn().TxStatus(); s != 'T' {
			t.Fatalf("after Record the caller's connection has the transaction status %q, want T (still open)", s)
		}
		if _, err := tx.Exec(f.ctx, "SELECT 1"); err != nil {
			t.Fatalf("the caller's transaction is not usable after Record: %v", err)
		}
		if err := tx.Rollback(f.ctx); err != nil {
			t.Fatal(err)
		}
		if n := f.auditRows(); n != 0 {
			t.Fatalf("%d audit rows after the rollback, want 0", n)
		}
	})

	t.Run("ST-SUP-RP05-LC02-T03 A denied or failed mutation is recorded alone with its outcome and error code", func(t *testing.T) {
		f := newPG(t)
		w := f.writer(&standInOutbox{})
		tx := f.begin()
		if _, err := tx.Exec(f.ctx, "INSERT INTO "+appTable+" (id) VALUES ('i-1')"); err != nil {
			t.Fatal(err)
		}
		if err := tx.Rollback(f.ctx); err != nil {
			t.Fatal(err)
		}
		log := &txLog{}
		for _, c := range []struct {
			outcome Outcome
			code    string
		}{{OutcomeDenied, "AUTHZ_DENIED"}, {OutcomeFailed, "STORE_CONFLICT"}} {
			e := sampleEvent("alpha", "reserve")
			e.Outcome, e.ErrorCode = c.outcome, c.code
			if _, err := w.RecordOutcome(f.ctx, f.runTx(log), e); err != nil {
				t.Fatalf("RecordOutcome %s: %v", c.outcome, err)
			}
		}
		for _, c := range []struct{ outcome, code string }{{"denied", "AUTHZ_DENIED"}, {"failed", "STORE_CONFLICT"}} {
			if n := f.count("SELECT count(*) FROM "+EventsTable+" WHERE outcome = $1 AND error_code = $2", c.outcome, c.code); n != 1 {
				t.Errorf("%d committed rows with the outcome %s and the error code %s, want 1", n, c.outcome, c.code)
			}
		}
		if n := f.count("SELECT count(*) FROM " + appTable); n != 0 {
			t.Errorf("the application row is in the table after the rollback")
		}
		if len(log.txs) != 2 {
			t.Fatalf("%d own transactions, want 2", len(log.txs))
		}
		for i, stmts := range log.txs {
			for _, s := range stmts {
				if !auditStatement(s) {
					t.Errorf("own transaction %d holds a statement that is not the audit write: %s", i+1, s)
				}
			}
		}
		e := sampleEvent("alpha", "reserve")
		if _, err := w.RecordOutcome(f.ctx, f.runTx(nil), e); err == nil {
			t.Errorf("RecordOutcome accepts the outcome ok")
		}
		e.Outcome = OutcomeFailed
		if _, err := w.RecordOutcome(f.ctx, f.runTx(nil), e); err == nil {
			t.Errorf("RecordOutcome accepts an outcome row with no error code")
		}
	})

	t.Run("ST-SUP-RP05-LC02-T04 A read of an entity that holds a pii field is recorded with the action read in its own transaction, and stays after a rollback", func(t *testing.T) {
		f := newPG(t)
		w := f.writer(&standInOutbox{})
		if _, err := f.owner.Exec(f.ctx, "INSERT INTO "+appTable+" (id, email) VALUES ('c-1', 'a@example.com'), ('c-2', 'b@example.com')"); err != nil {
			t.Fatal(err)
		}
		log := &txLog{}
		for i, id := range []string{"c-1", "c-2"} {
			tx := f.begin()
			var email string
			if err := tx.QueryRow(f.ctx, "SELECT email FROM "+appTable+" WHERE id = $1", id).Scan(&email); err != nil {
				t.Fatal(err)
			}
			e := sampleEvent("alpha", "")
			e.ResourceType, e.ResourceID = "customer", id
			// A service gives RecordRead its audit connections (ST-SUP-RP10-LC29); the test opens the own
			// transaction on the second runtime connection, apart from the caller's open transaction.
			if _, err := w.RecordRead(f.ctx, f.ownRunTx(log), e); err != nil {
				t.Fatalf("RecordRead: %v", err)
			}
			if i == 0 {
				if err := tx.Commit(f.ctx); err != nil {
					t.Fatal(err)
				}
			} else if err := tx.Rollback(f.ctx); err != nil {
				t.Fatal(err)
			}
		}
		for _, id := range []string{"c-1", "c-2"} {
			if n := f.count("SELECT count(*) FROM "+EventsTable+" WHERE action = 'read' AND resource_type = 'customer' AND resource_id = $1 AND outcome = 'ok'", id); n != 1 {
				t.Errorf("%d read rows for %s, want 1", n, id)
			}
		}
		for i, stmts := range log.txs {
			for _, s := range stmts {
				if !auditStatement(s) {
					t.Errorf("read transaction %d holds a statement that is not the audit write: %s", i+1, s)
				}
			}
		}
	})

	t.Run("ST-SUP-RP05-LC03-T06 A secret field does not reach the sink message", func(t *testing.T) {
		f := newPG(t)
		w := f.writer(&standInOutbox{})
		e := sampleEvent("alpha", "update")
		fields := Fields{"api_key": Secret}
		if err := e.SetStates(Settings{Level: LevelChange}, fields,
			map[string]any{"name": "a", "api_key": "sk-old-0001"}, map[string]any{"name": "b", "api_key": "sk-new-0002"}); err != nil {
			t.Fatal(err)
		}
		tx := f.begin()
		if _, err := w.Record(f.ctx, tx, e); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(f.ctx); err != nil {
			t.Fatal(err)
		}
		msgs := f.messages()
		if len(msgs) != 1 {
			t.Fatalf("%d outbox messages, want 1", len(msgs))
		}
		for _, needle := range []string{"api_key", "sk-old-0001", "sk-new-0002"} {
			if bytes.Contains(msgs[0].payload, []byte(needle)) {
				t.Errorf("the sink message payload holds %q: %s", needle, msgs[0].payload)
			}
		}
	})

	t.Run("ST-SUP-RP05-LC07-T02 The sink message key is the event ID", func(t *testing.T) {
		f := newPG(t)
		w := f.writer(&standInOutbox{})
		stored := f.recordN(w, "alpha", 1)[0]
		msgs := f.messages()
		if len(msgs) != 1 {
			t.Fatalf("%d outbox messages, want 1", len(msgs))
		}
		if msgs[0].key != stored.ID.String() {
			t.Errorf("the message key is %q, want the event ID %s", msgs[0].key, stored.ID)
		}
		if msgs[0].destination != SinkDestination {
			t.Errorf("the destination is %q, want %q", msgs[0].destination, SinkDestination)
		}
	})
}

// ownRunTx opens each own transaction on the second runtime connection, apart from the caller's.
func (f *pgFixture) ownRunTx(log *txLog) RunTx {
	return func(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error {
		return pgx.BeginFunc(ctx, f.other, func(tx pgx.Tx) error {
			rec := &recordingTx{Tx: tx}
			err := fn(ctx, rec)
			log.mu.Lock()
			log.txs = append(log.txs, rec.statements)
			log.mu.Unlock()
			return err
		})
	}
}
