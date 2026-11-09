package audit

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
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
	"github.com/jackc/pgx/v5/stdlib"

	"stego.dev/runtime/internal/pgtest"
	"stego.dev/runtime/lease"
	"stego.dev/runtime/tenant"
)

// The PostgreSQL tests run against the local PostgreSQL server of pgtest: the server that STEGO_TEST_POSTGRES_DSN
// names, a superuser URL, else a container without privilege that the test binary starts and removes
// (ST-STD-LOCAL-R04). They reach the server through pgx's database/sql driver (ST-STD-RUNTIME-R06). Each test
// makes a database of its own, owned by the owner role, applies the DDL text there as the owner, and drops the
// database at the end.

const (
	ownerRole     = "stego_audit_owner"
	runtimeRole   = "stego_audit_runtime"
	otherRole     = "stego_audit_other"
	rolePassword  = "stego-audit-test"
	outboxStandIn = "test_outbox"
	leaseName     = "controller"
	appTable      = "app_items"
	rejectMessage = "the audit table rejects"
	testRetention = 12
)

// TestMain gives the PostgreSQL tests of the package their local server (ST-STD-LOCAL-R04).
func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

var rolesOnce sync.Once

type pgFixture struct {
	t       *testing.T
	ctx     context.Context
	cfg     *pgx.ConnConfig
	db      string
	owner   *sql.DB
	runtime *sql.DB
	audit   *sql.DB // the audit connections of ST-SUP-RP10-LC29
	outbox  *standInOutbox
	trace   *statementTrace // every statement sent on the audit connections
}

type fixtureOption func(*Config)

// withClock sets the time that audit_maintain reads, as an SQL expression.
func withClock(sqlTime string) fixtureOption { return func(c *Config) { c.clock = sqlTime } }

func newPG(t *testing.T, opts ...fixtureOption) *pgFixture {
	t.Helper()
	dsn := pgtest.DSN(t)
	ctx := context.Background()
	base, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin := stdlib.OpenDB(*base)
	t.Cleanup(func() { admin.Close() })
	rolesOnce.Do(func() {
		for _, role := range []string{ownerRole, runtimeRole, otherRole} {
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
	f := &pgFixture{t: t, ctx: ctx, cfg: base, db: "audit_" + hex.EncodeToString(suffix[:]), outbox: &standInOutbox{}, trace: &statementTrace{}}
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+f.db+" OWNER "+ownerRole); err != nil {
		t.Fatalf("create the database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.ExecContext(ctx, "DROP DATABASE "+f.db+" WITH (FORCE)"); err != nil {
			t.Errorf("drop the database %s: %v", f.db, err)
		}
	})
	f.owner = f.open(ownerRole, nil)
	c := Config{RuntimeRole: runtimeRole, RetentionMonths: testRetention, AnchorPeriod: time.Hour}
	for _, o := range opts {
		o(&c)
	}
	ddl, err := DDL(c)
	if err != nil {
		t.Fatal(err)
	}
	setup := ddl + `
CREATE TABLE ` + outboxStandIn + ` (n bigserial PRIMARY KEY, tenant_id text NOT NULL, destination text NOT NULL,
  key text NOT NULL, payload bytea NOT NULL);
GRANT SELECT, INSERT ON ` + outboxStandIn + ` TO ` + runtimeRole + `;
GRANT USAGE ON SEQUENCE ` + outboxStandIn + `_n_seq TO ` + runtimeRole + `;
 ` + lease.LeasesTable + `;
INSERT INTO leases (name) VALUES ('` + leaseName + `');
GRANT SELECT, UPDATE ON leases TO ` + runtimeRole + `;
CREATE TABLE test_deferred (x int UNIQUE DEFERRABLE INITIALLY DEFERRED);
GRANT INSERT ON test_deferred TO ` + runtimeRole + `;
CREATE TABLE ` + appTable + ` (id text PRIMARY KEY, email text);
GRANT SELECT, INSERT ON ` + appTable + ` TO ` + runtimeRole + `;`
	if _, err := f.owner.ExecContext(ctx, setup); err != nil {
		t.Fatalf("apply the DDL text: %v", err)
	}
	f.runtime = f.open(runtimeRole, nil)
	f.audit = f.open(runtimeRole, f.trace)
	f.maintainAll()
	return f
}

func (f *pgFixture) open(role string, trace *statementTrace) *sql.DB {
	cfg := f.cfg.Copy()
	cfg.Database, cfg.User, cfg.Password = f.db, role, rolePassword
	cfg.RuntimeParams = map[string]string{"TimeZone": "UTC"}
	if trace != nil {
		cfg.Tracer = trace
	}
	db := stdlib.OpenDB(*cfg)
	f.t.Cleanup(func() { db.Close() })
	return db
}

// maintainAll calls audit_maintain() as the runtime role until it reports no step left, and returns the calls.
func (f *pgFixture) maintainAll() int {
	f.t.Helper()
	calls := 0
	for ; calls < 64; calls++ {
		var more bool
		err := f.inTx(f.runtime, "", func(tx *sql.Tx) error {
			rows, err := tx.QueryContext(f.ctx, callMaintain)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var tenant sql.NullString
				var seq sql.NullInt64
				var hash []byte
				if err := rows.Scan(&more, &tenant, &seq, &hash); err != nil {
					return err
				}
			}
			return rows.Err()
		})
		if err != nil {
			f.t.Fatalf("audit_maintain: %v", err)
		}
		if !more {
			return calls + 1
		}
	}
	f.t.Fatal("audit_maintain reports a step left after 64 calls")
	return calls
}

// tn returns the tenant of the ID, built as the tenant runtime builds a tenant from a column that a statement
// read (tenant.ScanRows).
func (f *pgFixture) tn(id string) tenant.Tenant {
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

// inTx runs fn in a transaction of db. With a tenant it opens the transaction with tenant.Begin and commits it
// with tenant.Commit, which checks the tenant setting (ST-SUP-RP04-LC03); with no tenant it checks that the
// setting is still empty before the commit.
func (f *pgFixture) inTx(db *sql.DB, id string, fn func(tx *sql.Tx) error) error {
	if id == "" {
		return noTenant(f.ctx, db, fn)
	}
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

// begin opens a caller transaction of the tenant on the runtime pool with tenant.Begin.
func (f *pgFixture) begin(id string) *sql.Tx {
	f.t.Helper()
	tx, err := tenant.Begin(f.ctx, f.runtime, f.tn(id), nil)
	if err != nil {
		f.t.Fatal(err)
	}
	return tx
}

// commit commits a caller transaction with tenant.Commit.
func (f *pgFixture) commit(tx *sql.Tx, id string) error {
	return tenant.Commit(f.ctx, tx, f.tn(id))
}

// statementTrace records every statement that the connections of a pool send.
type statementTrace struct {
	mu   sync.Mutex
	sqls []string
}

func (s *statementTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	s.mu.Lock()
	s.sqls = append(s.sqls, data.SQL)
	s.mu.Unlock()
	return ctx
}

func (s *statementTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (s *statementTrace) mark() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sqls)
}

// transactions returns the statements sent since the mark, cut into transactions at each begin.
func (s *statementTrace) transactions(mark int) [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out [][]string
	for _, q := range s.sqls[mark:] {
		if strings.EqualFold(strings.TrimSpace(q), "begin") {
			out = append(out, nil)
			continue
		}
		if len(out) > 0 {
			out[len(out)-1] = append(out[len(out)-1], q)
		}
	}
	return out
}

type recordingTx struct {
	*sql.Tx
	mu         sync.Mutex
	statements []string
}

func (r *recordingTx) add(q string) {
	r.mu.Lock()
	r.statements = append(r.statements, q)
	r.mu.Unlock()
}

func (r *recordingTx) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	r.add(q)
	return r.Tx.ExecContext(ctx, q, args...)
}

func (r *recordingTx) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	r.add(q)
	return r.Tx.QueryRowContext(ctx, q, args...)
}

func (r *recordingTx) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	r.add(q)
	return r.Tx.QueryContext(ctx, q, args...)
}

// standInOutbox is the outbox stand-in: it writes the message, with its tenant, into a table through the caller's
// transaction, as the outbox runtime's queue does, until that runtime is on main.
type standInOutbox struct {
	mu        sync.Mutex
	fail      bool
	onEnqueue func()
	onTx      func(ctx context.Context, tx Tx) error
}

func (o *standInOutbox) Enqueue(ctx context.Context, tx Tx, tenant, destination, key string, payload []byte) error {
	o.mu.Lock()
	fail, hook, inTx := o.fail, o.onEnqueue, o.onTx
	o.mu.Unlock()
	if hook != nil {
		hook()
	}
	if inTx != nil {
		if err := inTx(ctx, tx); err != nil {
			return err
		}
	}
	if fail {
		return errors.New("the outbox refuses the message")
	}
	_, err := tx.ExecContext(ctx, "INSERT INTO "+outboxStandIn+" (tenant_id, destination, key, payload) VALUES ($1, $2, $3, $4)",
		tenant, destination, key, payload)
	return err
}

func (o *standInOutbox) setFail(v bool) {
	o.mu.Lock()
	o.fail = v
	o.mu.Unlock()
}

type outboxMessage struct {
	tenant, destination, key string
	payload                  []byte
}

func (f *pgFixture) messages() []outboxMessage {
	f.t.Helper()
	rows, err := f.owner.QueryContext(f.ctx, "SELECT tenant_id, destination, key, payload FROM "+outboxStandIn+" ORDER BY n")
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []outboxMessage
	for rows.Next() {
		var m outboxMessage
		if err := rows.Scan(&m.tenant, &m.destination, &m.key, &m.payload); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

// count runs a count query as the owner in a transaction of the tenant ("" for none).
func (f *pgFixture) count(tenant, q string, args ...any) int64 {
	f.t.Helper()
	var n int64
	if err := f.inTx(f.owner, tenant, func(tx *sql.Tx) error { return tx.QueryRowContext(f.ctx, q, args...).Scan(&n) }); err != nil {
		f.t.Fatalf("%s: %v", q, err)
	}
	return n
}

func (f *pgFixture) auditRows(tenant string) int64 {
	return f.count(tenant, "SELECT count(*) FROM "+EventsTable)
}

func (f *pgFixture) writer() *Writer {
	f.t.Helper()
	w, err := NewWriter(f.outbox, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	return w
}

func sampleEvent(tenant, action string) Event {
	return Event{
		TenantID:     tenant,
		ActorID:      "user:u-7",
		ActorKind:    ActorUser,
		ActorIP:      "203.0.113.9",
		UserAgent:    "test-agent/1.0",
		Action:       action,
		ResourceType: "item",
		ResourceID:   "i-1",
		Outcome:      OutcomeOK,
		RequestID:    "r-1",
		Service:      "orders",
	}
}

// recordN writes n committed rows of the tenant, one transaction each.
func (f *pgFixture) recordN(w *Writer, tenant string, n int) []Event {
	f.t.Helper()
	var out []Event
	for i := 0; i < n; i++ {
		tx := f.begin(tenant)
		e := sampleEvent(tenant, fmt.Sprintf("update-%d", i+1))
		e.ResourceID = fmt.Sprintf("i-%d", i+1)
		e.RequestID = fmt.Sprintf("r-%s-%d-%d", tenant, time.Now().UnixNano(), i)
		stored, err := w.Record(f.ctx, tx, f.tn(tenant), []Event{e})
		if err != nil {
			f.t.Fatalf("Record: %v", err)
		}
		if err := f.commit(tx, tenant); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, stored...)
	}
	return out
}

// seed writes linked rows of the tenant with the given recorded_at values as the owner, straight into the table,
// as rows of an earlier month would be, and moves the chain head.
func (f *pgFixture) seed(tenant string, times []time.Time) []Event {
	f.t.Helper()
	var out []Event
	err := f.inTx(f.owner, tenant, func(tx *sql.Tx) error {
		var seq int64
		prev := ZeroHash()
		var last time.Time
		err := tx.QueryRowContext(f.ctx, "SELECT seq, head, last_recorded_at FROM "+HeadsTable+" WHERE tenant_id = $1", tenant).Scan(&seq, &prev, &last)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		for i, at := range times {
			e, err := New(sampleEvent(tenant, fmt.Sprintf("seeded-%d", i+1)), at)
			if err != nil {
				return err
			}
			e.RecordedAt = storedTime(at)
			if e, err = Link(e, prev); err != nil {
				return err
			}
			seq++
			if _, err := tx.ExecContext(f.ctx, insertEvent, tenant, seq, e.ID.String(), e.OccurredAt, e.RecordedAt,
				e.ActorID, string(e.ActorKind), string(e.ActorIP), string(e.UserAgent), e.Action, e.ResourceType,
				e.ResourceID, string(e.Outcome), e.ErrorCode, e.RequestID, e.TraceID, e.SpanID, e.IdempotencyKey,
				nil, nil, nil, e.Service, e.ServiceVersion, e.DeclarationVersion, e.PrevHash, e.Hash); err != nil {
				return err
			}
			prev = e.Hash
			last = e.RecordedAt
			out = append(out, e)
		}
		_, err = tx.ExecContext(f.ctx, `INSERT INTO `+HeadsTable+` (tenant_id, seq, head, last_recorded_at) VALUES ($1, $2, $3, $4)
 ON CONFLICT (tenant_id) DO UPDATE SET seq = EXCLUDED.seq, head = EXCLUDED.head, last_recorded_at = EXCLUDED.last_recorded_at`,
			tenant, seq, prev, last)
		return err
	})
	if err != nil {
		f.t.Fatalf("seed: %v", err)
	}
	return out
}

// createMonth creates the partition of a month as the owner, through the owner's partition function.
func (f *pgFixture) createMonth(month time.Time) {
	f.t.Helper()
	if _, err := f.owner.ExecContext(f.ctx, "SELECT audit_create_partition($1::date)", month.Format("2006-01-02")); err != nil {
		f.t.Fatalf("create the partition of %s: %v", month.Format("2006-01"), err)
	}
}

func auditStatement(q string) bool {
	q = strings.TrimSpace(q)
	return strings.Contains(q, EventsTable) || strings.Contains(q, HeadsTable) || strings.Contains(q, outboxStandIn) ||
		strings.Contains(q, "lock_timeout") || strings.Contains(q, "statement_timeout") || strings.Contains(q, "app.tenant") ||
		strings.EqualFold(q, "commit") || strings.EqualFold(q, "rollback")
}

func containsAny(s string, needles ...string) string {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return n
		}
	}
	return ""
}

func TestRecord(t *testing.T) {
	t.Run("ST-SUP-RP05-LC02-T01 An audit entry is visible only after the caller commits", func(t *testing.T) {
		f := newPG(t)
		w := f.writer()
		tx := f.begin("t1")
		if _, err := tx.ExecContext(f.ctx, "INSERT INTO "+appTable+" (id) VALUES ('i-1')"); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Record(f.ctx, tx, f.tn("t1"), []Event{sampleEvent("t1", "create")}); err != nil {
			t.Fatal(err)
		}
		if n := f.auditRows("t1"); n != 0 {
			t.Fatalf("a second session sees %d audit rows before the commit, want 0", n)
		}
		if err := f.commit(tx, "t1"); err != nil {
			t.Fatal(err)
		}
		if n := f.auditRows("t1"); n != 1 {
			t.Fatalf("a second session sees %d audit rows after the commit, want 1", n)
		}
	})

	t.Run("ST-SUP-RP05-LC02-T02 A rolled-back caller transaction leaves no audit row", func(t *testing.T) {
		f := newPG(t)
		w := f.writer()
		tx := f.begin("t1")
		if _, err := w.Record(f.ctx, tx, f.tn("t1"), []Event{sampleEvent("t1", "create")}); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(f.ctx, "SELECT 1"); err != nil {
			t.Fatalf("the caller's transaction is not open after Record: %v", err)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatalf("the caller's transaction cannot roll back, so Record ended it: %v", err)
		}
		if n := f.auditRows("t1"); n != 0 {
			t.Fatalf("%d audit rows after the rollback, want 0", n)
		}
	})

	t.Run("ST-SUP-RP05-LC02-T03 A denied or failed mutation is recorded alone with its outcome and error code", func(t *testing.T) {
		f := newPG(t)
		w := f.writer()
		tx := f.begin("t1")
		if _, err := tx.ExecContext(f.ctx, "INSERT INTO "+appTable+" (id) VALUES ('i-1')"); err != nil {
			t.Fatal(err)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
		mark := f.trace.mark()
		for _, c := range []struct {
			outcome Outcome
			code    string
		}{{OutcomeDenied, "AUTHZ_DENIED"}, {OutcomeFailed, "STORE_CONFLICT"}} {
			e := sampleEvent("t1", "reserve")
			e.Outcome, e.ErrorCode = c.outcome, c.code
			if _, err := w.RecordOutcome(f.ctx, f.audit, f.tn("t1"), e); err != nil {
				t.Fatalf("RecordOutcome %s: %v", c.outcome, err)
			}
		}
		for _, c := range []struct{ outcome, code string }{{"denied", "AUTHZ_DENIED"}, {"failed", "STORE_CONFLICT"}} {
			if n := f.count("t1", "SELECT count(*) FROM "+EventsTable+" WHERE outcome = $1 AND error_code = $2", c.outcome, c.code); n != 1 {
				t.Errorf("%d committed rows with the outcome %s and the error code %s, want 1", n, c.outcome, c.code)
			}
		}
		if n := f.count("", "SELECT count(*) FROM "+appTable); n != 0 {
			t.Errorf("the application row is in the table after the rollback")
		}
		txs := f.trace.transactions(mark)
		if len(txs) != 2 {
			t.Fatalf("%d own transactions, want 2", len(txs))
		}
		for i, stmts := range txs {
			for _, s := range stmts {
				if !auditStatement(s) {
					t.Errorf("own transaction %d holds a statement that is not the audit write: %s", i+1, s)
				}
			}
		}
		e := sampleEvent("t1", "reserve")
		if _, err := w.RecordOutcome(f.ctx, f.audit, f.tn("t1"), e); err == nil {
			t.Errorf("RecordOutcome accepts the outcome ok")
		}
		e.Outcome = OutcomeFailed
		if _, err := w.RecordOutcome(f.ctx, f.audit, f.tn("t1"), e); err == nil {
			t.Errorf("RecordOutcome accepts an outcome row with no error code")
		}
	})

	t.Run("ST-SUP-RP05-LC02-T04 A read of an entity that holds a pii field is recorded with the action read in its own transaction, and stays after a rollback", func(t *testing.T) {
		f := newPG(t)
		w := f.writer()
		if _, err := f.owner.ExecContext(f.ctx, "INSERT INTO "+appTable+" (id, email) VALUES ('c-1', 'a@example.com'), ('c-2', 'b@example.com')"); err != nil {
			t.Fatal(err)
		}
		mark := f.trace.mark()
		for i, id := range []string{"c-1", "c-2"} {
			tx := f.begin("t1")
			var email string
			if err := tx.QueryRowContext(f.ctx, "SELECT email FROM "+appTable+" WHERE id = $1", id).Scan(&email); err != nil {
				t.Fatal(err)
			}
			e := sampleEvent("t1", "")
			e.ResourceType, e.ResourceID = "customer", id
			if _, err := w.RecordRead(f.ctx, f.audit, f.tn("t1"), e); err != nil {
				t.Fatalf("RecordRead: %v", err)
			}
			if i == 0 {
				if err := f.commit(tx, "t1"); err != nil {
					t.Fatal(err)
				}
			} else if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
		}
		for _, id := range []string{"c-1", "c-2"} {
			if n := f.count("t1", "SELECT count(*) FROM "+EventsTable+" WHERE action = 'read' AND resource_type = 'customer' AND resource_id = $1 AND outcome = 'ok'", id); n != 1 {
				t.Errorf("%d read rows for %s, want 1", n, id)
			}
		}
		for i, stmts := range f.trace.transactions(mark) {
			for _, s := range stmts {
				if !auditStatement(s) {
					t.Errorf("read transaction %d holds a statement that is not the audit write: %s", i+1, s)
				}
			}
		}
	})

	t.Run("ST-SUP-RP05-LC03-T06 A secret field does not reach the sink message", func(t *testing.T) {
		f := newPG(t)
		w := f.writer()
		e := sampleEvent("t1", "update")
		if err := e.SetStates(Settings{Level: LevelChange}, Fields{"api_key": Secret},
			map[string]any{"name": "a", "api_key": "sk-old-0001"}, map[string]any{"name": "b", "api_key": "sk-new-0002"}); err != nil {
			t.Fatal(err)
		}
		tx := f.begin("t1")
		if _, err := w.Record(f.ctx, tx, f.tn("t1"), []Event{e}); err != nil {
			t.Fatal(err)
		}
		if err := f.commit(tx, "t1"); err != nil {
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
		stored := f.recordN(f.writer(), "t1", 1)[0]
		msgs := f.messages()
		if len(msgs) != 1 {
			t.Fatalf("%d outbox messages, want 1", len(msgs))
		}
		if msgs[0].key != stored.ID.String() {
			t.Errorf("the message key is %q, want the event ID %s", msgs[0].key, stored.ID)
		}
		if msgs[0].destination != SinkDestination || msgs[0].tenant != "t1" {
			t.Errorf("the message goes to %q for the tenant %q, want %q for t1", msgs[0].destination, msgs[0].tenant, SinkDestination)
		}
	})

	t.Run("ST-SUP-RP05-LC12-T01 Record gives a batch of events consecutive seq values under one lock", func(t *testing.T) {
		f := newPG(t)
		w := f.writer()
		f.recordN(w, "t1", 2)
		tx := f.begin("t1")
		rec := &recordingTx{Tx: tx}
		if _, err := rec.ExecContext(f.ctx, "INSERT INTO "+appTable+" (id) VALUES ('i-9')"); err != nil {
			t.Fatal(err)
		}
		var batch []Event
		for i := 0; i < 3; i++ {
			e := sampleEvent("t1", fmt.Sprintf("batch-%d", i+1))
			batch = append(batch, e)
		}
		stored, err := w.Record(f.ctx, rec, f.tn("t1"), batch)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.commit(tx, "t1"); err != nil {
			t.Fatal(err)
		}
		var seqs []int64
		if err := f.inTx(f.owner, "t1", func(tx *sql.Tx) error {
			r, err := tx.QueryContext(f.ctx, "SELECT seq, action FROM "+EventsTable+" WHERE action LIKE 'batch-%' ORDER BY seq")
			if err != nil {
				return err
			}
			defer r.Close()
			for i := 0; r.Next(); i++ {
				var seq int64
				var action string
				if err := r.Scan(&seq, &action); err != nil {
					return err
				}
				if action != stored[i].Action {
					t.Errorf("seq %d holds %s, want %s in the order given", seq, action, stored[i].Action)
				}
				seqs = append(seqs, seq)
			}
			return r.Err()
		}); err != nil {
			t.Fatal(err)
		}
		if len(seqs) != 3 || seqs[0] != 3 || seqs[1] != 4 || seqs[2] != 5 {
			t.Errorf("the batch has the seq values %v, want [3 4 5]", seqs)
		}
		locks, lockAt := 0, -1
		for i, s := range rec.statements {
			if strings.Contains(s, "INSERT INTO "+HeadsTable) {
				locks++
				lockAt = i
			}
		}
		if locks != 1 {
			t.Fatalf("%d locks of the chain head, want 1", locks)
		}
		for _, s := range rec.statements[lockAt+1:] {
			if !auditStatement(s) || strings.Contains(s, appTable) {
				t.Errorf("a statement of the caller runs after the chain lock: %s", s)
			}
		}
	})

	t.Run("ST-SUP-RP05-LC12-T02 recorded_at is set after the lock and never falls along a chain", func(t *testing.T) {
		f := newPG(t)
		w := f.writer()
		f.recordN(w, "t1", 1)
		var dbNow time.Time
		if err := f.owner.QueryRowContext(f.ctx, "SELECT clock_timestamp()").Scan(&dbNow); err != nil {
			t.Fatal(err)
		}
		ahead := dbNow.Add(time.Hour).Truncate(time.Microsecond)
		if err := f.inTx(f.owner, "t1", func(tx *sql.Tx) error {
			_, err := tx.ExecContext(f.ctx, "UPDATE "+HeadsTable+" SET last_recorded_at = $1 WHERE tenant_id = 't1'", ahead)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		e := sampleEvent("t1", "late")
		e.RecordedAt = time.Now().Add(-24 * time.Hour)
		tx := f.begin("t1")
		stored, err := w.Record(f.ctx, tx, f.tn("t1"), []Event{e})
		if err != nil {
			t.Fatal(err)
		}
		if err := f.commit(tx, "t1"); err != nil {
			t.Fatal(err)
		}
		var got time.Time
		if err := f.inTx(f.owner, "t1", func(tx *sql.Tx) error {
			return tx.QueryRowContext(f.ctx, "SELECT recorded_at FROM "+EventsTable+" WHERE action = 'late'").Scan(&got)
		}); err != nil {
			t.Fatal(err)
		}
		if got.Before(ahead) || !got.Equal(stored[0].RecordedAt) {
			t.Errorf("recorded_at is %s (returned %s), want the chain's last recorded_at %s or later", got, stored[0].RecordedAt, ahead)
		}
	})

	t.Run("ST-SUP-RP05-LC12-T03 A read row of a fill does not wait on its own command", func(t *testing.T) {
		f := newPG(t)
		w := f.writer()
		// The chain head of t1 exists first, so a command that took the chain lock before the fill returned
		// would make the read row wait and fail this test.
		f.recordN(w, "t1", 1)
		cmd := f.begin("t1")
		if _, err := cmd.ExecContext(f.ctx, "SET LOCAL lock_timeout = '2s'"); err != nil {
			t.Fatal(err)
		}
		if _, err := cmd.ExecContext(f.ctx, "INSERT INTO "+appTable+" (id, email) VALUES ('c-1', 'a@example.com')"); err != nil {
			t.Fatal(err)
		}
		held := sampleEvent("t1", "create") // buffered, not written yet (ST-SUP-RP10-LC06)
		read := sampleEvent("t1", "")
		read.ResourceType, read.ResourceID = "customer", "c-0"
		start := time.Now()
		if _, err := w.RecordRead(f.ctx, f.audit, f.tn("t1"), read); err != nil {
			t.Fatalf("the read row fails while its command is open: %v", err)
		}
		if d := time.Since(start); d > time.Second {
			t.Errorf("the read row waited %s", d)
		}
		if _, err := w.Record(f.ctx, cmd, f.tn("t1"), []Event{held}); err != nil {
			t.Fatal(err)
		}
		if err := f.commit(cmd, "t1"); err != nil {
			t.Fatal(err)
		}
		var actions []string
		if err := f.inTx(f.owner, "t1", func(tx *sql.Tx) error {
			r, err := tx.QueryContext(f.ctx, "SELECT seq, action FROM "+EventsTable+" ORDER BY seq")
			if err != nil {
				return err
			}
			defer r.Close()
			for want := int64(1); r.Next(); want++ {
				var seq int64
				var a string
				if err := r.Scan(&seq, &a); err != nil {
					return err
				}
				if seq != want {
					t.Errorf("seq %d follows %d", seq, want-1)
				}
				actions = append(actions, a)
			}
			return r.Err()
		}); err != nil {
			t.Fatal(err)
		}
		if strings.Join(actions, ",") != "update-1,read,create" {
			t.Errorf("the chain holds %v, want the read row and then the write row", actions)
		}
	})

	t.Run("ST-SUP-RP05-LC12-T04 A read row after its own command's Record fails at once", func(t *testing.T) {
		f := newPG(t)
		w := f.writer()
		f.recordN(w, "t1", 1)
		cmd := f.begin("t1")
		defer cmd.Rollback()
		write := sampleEvent("t1", "create")
		write.RequestID = "req-42"
		if _, err := w.Record(f.ctx, cmd, f.tn("t1"), []Event{write}); err != nil {
			t.Fatal(err)
		}
		read := sampleEvent("t1", "")
		read.RequestID = "req-42"
		start := time.Now()
		_, err := w.RecordRead(f.ctx, f.audit, f.tn("t1"), read)
		if !errors.Is(err, ErrReadAfterRecord) {
			t.Errorf("RecordRead after Record of the same request gives %v, want ErrReadAfterRecord", err)
		}
		if d := time.Since(start); d > 200*time.Millisecond {
			t.Errorf("RecordRead waited %s, want it to fail at once", d)
		}
		// A read row of another request is not refused; it waits on the lock as any writer does.
		other := sampleEvent("t1", "")
		other.RequestID = "req-43"
		if _, err := w.RecordRead(f.ctx, f.audit, f.tn("t1"), other); errors.Is(err, ErrReadAfterRecord) {
			t.Errorf("RecordRead of another request is refused as a read after Record")
		}
	})

	t.Run("ST-SUP-RP05-LC13-T01 Bad bytes in an untrusted value do not fail the audited write", func(t *testing.T) {
		f := newPG(t)
		w := f.writer()
		e := sampleEvent("t1", "update")
		e.UserAgent = Untrusted("bad\xff\xfe\x00end")
		e.ActorIP = Untrusted(strings.Repeat("é", 300)) // 600 bytes

		tx := f.begin("t1")
		stored, err := w.Record(f.ctx, tx, f.tn("t1"), []Event{e})
		if err != nil {
			t.Fatalf("Record fails on bad untrusted bytes: %v", err)
		}
		if err := f.commit(tx, "t1"); err != nil {
			t.Fatal(err)
		}
		var ua, ip string
		if err := f.inTx(f.owner, "t1", func(tx *sql.Tx) error {
			return tx.QueryRowContext(f.ctx, "SELECT user_agent, actor_ip FROM "+EventsTable).Scan(&ua, &ip)
		}); err != nil {
			t.Fatal(err)
		}
		if want := "bad��end"; ua != want || string(stored[0].UserAgent) != want {
			t.Errorf("the stored user agent is %q (hashed %q), want %q", ua, stored[0].UserAgent, want)
		}
		if len(ip) > MaxUntrusted || !utf8Valid(ip) || ip != string(stored[0].ActorIP) || len(ip) < MaxUntrusted-1 {
			t.Errorf("the stored address has %d bytes, valid %v, want at most %d bytes of valid UTF-8", len(ip), utf8Valid(ip), MaxUntrusted)
		}
		if h, _ := Hash(stored[0]); !bytes.Equal(h, stored[0].Hash) {
			t.Errorf("the hash does not cover the cleaned values")
		}
	})

	t.Run("ST-SUP-RP05-LC13-T06 Bad bytes in a request ID or an idempotency key do not fail the audited write", func(t *testing.T) {
		f := newPG(t)
		w := f.writer()
		e := sampleEvent("t1", "update")
		e.RequestID = "req\x00\xff-1"
		e.IdempotencyKey = strings.Repeat("k", 599) + "\xfe"
		tx := f.begin("t1")
		stored, err := w.Record(f.ctx, tx, f.tn("t1"), []Event{e})
		if err != nil {
			t.Fatalf("Record fails on bad request values: %v", err)
		}
		if err := f.commit(tx, "t1"); err != nil {
			t.Fatal(err)
		}
		var rid, key string
		if err := f.inTx(f.owner, "t1", func(tx *sql.Tx) error {
			return tx.QueryRowContext(f.ctx, "SELECT request_id, idempotency_key FROM "+EventsTable).Scan(&rid, &key)
		}); err != nil {
			t.Fatal(err)
		}
		if rid != "req\uFFFD-1" || rid != stored[0].RequestID {
			t.Errorf("the stored request ID is %q (hashed %q), want %q", rid, stored[0].RequestID, "req\uFFFD-1")
		}
		if len(key) > MaxUntrusted || !utf8Valid(key) || key != stored[0].IdempotencyKey || strings.ContainsRune(key, 0) {
			t.Errorf("the stored key has %d bytes, valid %v, want at most %d bytes of valid UTF-8", len(key), utf8Valid(key), MaxUntrusted)
		}
		if h, _ := Hash(stored[0]); !bytes.Equal(h, stored[0].Hash) {
			t.Errorf("the hash does not cover the cleaned values")
		}
	})

	t.Run("ST-SUP-RP05-LC13-T04 RecordOutcome and RecordRead give each event a new ID", func(t *testing.T) {
		f := newPG(t)
		w := f.writer()
		given, err := NewID(time.Now())
		if err != nil {
			t.Fatal(err)
		}
		e := sampleEvent("t1", "reserve")
		e.ID = given
		e.Outcome, e.ErrorCode = OutcomeFailed, ErrorCodeConflict
		var ids []UUID
		for i := 0; i < 2; i++ {
			s, err := w.RecordOutcome(f.ctx, f.audit, f.tn("t1"), e)
			if err != nil {
				t.Fatal(err)
			}
			ids = append(ids, s.ID)
		}
		s, err := w.RecordRead(f.ctx, f.audit, f.tn("t1"), e)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, s.ID)
		if n := f.auditRows("t1"); n != 3 {
			t.Errorf("%d rows, want 3", n)
		}
		if n := f.count("t1", "SELECT count(*) FROM "+EventsTable+" WHERE id = $1::uuid", given.String()); n != 0 {
			t.Errorf("%d rows have the given ID", n)
		}
		seen := map[UUID]bool{}
		for _, id := range ids {
			if id == given || seen[id] || id[6]>>4 != 7 {
				t.Errorf("the ID %s is not a new UUID version 7", id)
			}
			seen[id] = true
		}
	})

	t.Run("ST-SUP-RP05-LC13-T05 A failed RecordOutcome is counted and names no value", func(t *testing.T) {
		f := newPG(t)
		w := f.writer()
		blocker := f.begin("t1")
		if _, err := w.Record(f.ctx, blocker, f.tn("t1"), []Event{sampleEvent("t1", "create")}); err != nil {
			t.Fatal(err)
		}
		defer blocker.Rollback()
		e := sampleEvent("t1", "reserve")
		e.Outcome, e.ErrorCode = OutcomeDenied, "AUTHZ_DENIED"
		e.ResourceID = "secret-resource-7781"
		e.UserAgent = "agent-value-4410"
		if err := e.SetStates(Settings{}, Fields{"email": PII}, nil, map[string]any{"email": "x@example.com", "note": "note-value-9031"}); err != nil {
			t.Fatal(err)
		}
		_, err := w.RecordOutcome(f.ctx, f.audit, f.tn("t1"), e)
		if err == nil {
			t.Fatal("RecordOutcome succeeds while the chain head is locked")
		}
		if v := containsAny(err.Error(), "secret-resource-7781", "agent-value-4410", "x@example.com", "note-value-9031"); v != "" {
			t.Errorf("the error %q holds the value %q", err, v)
		}
		if sqlState(err) != "55P03" {
			t.Errorf("the error has the SQLSTATE %q, want 55P03", sqlState(err))
		}
		if n := w.FailedOutcomes(); n != 1 {
			t.Errorf("the failed-outcome counter is %d, want 1", n)
		}
	})

	t.Run("ST-SUP-RP05-LC04-T06 Concurrent writers of one tenant keep one chain", func(t *testing.T) {
		f := newPG(t)
		w := f.writer()
		const writers, each = 8, 5
		var wg sync.WaitGroup
		errs := make(chan error, writers*each)
		for g := 0; g < writers; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				for i := 0; i < each; i++ {
					err := f.inTx(f.runtime, "t1", func(tx *sql.Tx) error {
						_, err := w.Record(f.ctx, tx, f.tn("t1"), []Event{sampleEvent("t1", fmt.Sprintf("w%d-%d", g, i)), sampleEvent("t1", "second")})
						return err
					})
					if err != nil {
						errs <- err
					}
				}
			}(g)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("a concurrent writer fails: %v", err)
		}
		var findings []Finding
		if err := f.inTx(f.runtime, "t1", func(tx *sql.Tx) error {
			var err error
			findings, err = Verify(f.ctx, tx, f.tn("t1"), nil)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if len(findings) != 0 {
			t.Errorf("findings after concurrent writers: %v", findings)
		}
		if n := f.count("t1", "SELECT max(seq) FROM "+EventsTable); n != writers*each*2 {
			t.Errorf("the highest seq is %d, want %d", n, writers*each*2)
		}
	})
}

func utf8Valid(s string) bool { return strings.ToValidUTF8(s, "") == s }
