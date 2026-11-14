package outbox

// The tests of the outbox wiring on the runtime (ST-SUP-RC18-OC30). They run in the generated package of a service
// with the audit-log and idempotency components, against the PostgreSQL server that STEGO_TEST_POSTGRES_DSN names,
// a superuser URL (ST-STD-LOCAL). Each test makes a database of its own, owned by an owner role that is a member of
// the relay role, applies the emitted migrations there as the owner, and drops the database at the end.

import (
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

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/jsell-rh/stego/registry/runtime/audit"
	"github.com/jsell-rh/stego/registry/runtime/outbox"
	"github.com/jsell-rh/stego/registry/runtime/tenant"

	"example.com/orders/internal/auditlog"
	"example.com/orders/internal/idempotency"
)

const rolePassword = "stego-outbox-test"

// env is one database with the roles of the service: owner, runtime and worker.
type env struct {
	t                       *testing.T
	ctx                     context.Context
	cfg                     *pgx.ConnConfig
	name                    string
	runtimeRole, workerRole string
	purgeRole, relayRole    string
	owner, runtime, worker  *sql.DB
}

func newEnv(t *testing.T) *env {
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
	var suffix [5]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	tag := hex.EncodeToString(suffix[:])
	e := &env{t: t, ctx: ctx, cfg: base, name: "outbox_" + tag,
		runtimeRole: "ob_runtime_" + tag, workerRole: "ob_worker_" + tag}
	ownerRole := "ob_owner_" + tag
	exec := func(statement string) {
		t.Helper()
		if _, err := admin.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	for _, role := range []string{ownerRole, e.runtimeRole, e.workerRole} {
		exec(fmt.Sprintf("CREATE ROLE %s LOGIN NOSUPERUSER NOBYPASSRLS NOINHERIT PASSWORD '%s'", role, rolePassword))
		t.Cleanup(func() { _, _ = admin.ExecContext(context.Background(), "DROP ROLE IF EXISTS "+role) })
	}
	// The outbox relay role is one role of this service; the owner makes the relay functions as it with SET ROLE, as
	// the tenancy role migration allows (SET TRUE, INHERIT FALSE; ST-SUP-RP01-LC19).
	e.relayRole = "ob_" + tag + outbox.RelaySuffix
	exec("CREATE ROLE " + e.relayRole + " NOLOGIN NOINHERIT")
	exec("GRANT " + e.relayRole + " TO " + ownerRole + " WITH SET TRUE, INHERIT FALSE")
	t.Cleanup(func() { _, _ = admin.ExecContext(context.Background(), "DROP ROLE IF EXISTS "+e.relayRole) })
	// The idempotency purge role is one role of this service; the owner makes the purge function as it with SET
	// ROLE, as the tenancy role migration allows (SET TRUE, INHERIT FALSE; ST-SUP-RP07-LC11).
	e.purgeRole = "ob_" + tag + "_idempotency_purge"
	exec("CREATE ROLE " + e.purgeRole + " NOLOGIN NOINHERIT")
	exec("GRANT " + e.purgeRole + " TO " + ownerRole + " WITH SET TRUE, INHERIT FALSE")
	t.Cleanup(func() { _, _ = admin.ExecContext(context.Background(), "DROP ROLE IF EXISTS "+e.purgeRole) })
	exec("CREATE DATABASE " + e.name + " OWNER " + ownerRole)
	// The drop gets 30 seconds, and a drop that fails is logged: the cleanup alone never fails the test.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := admin.ExecContext(ctx, "DROP DATABASE "+e.name+" WITH (FORCE)"); err != nil {
			t.Logf("drop the database %s: %v", e.name, err)
		}
	})
	e.owner = e.open(ownerRole, rolePassword)
	e.runtime = e.open(e.runtimeRole, rolePassword)
	e.worker = e.open(e.workerRole, rolePassword)
	return e
}

func (e *env) open(role, password string) *sql.DB {
	cfg := e.cfg.Copy()
	cfg.Database, cfg.User, cfg.Password = e.name, role, password
	cfg.RuntimeParams = map[string]string{"TimeZone": "UTC"}
	db := stdlib.OpenDB(*cfg)
	e.t.Cleanup(func() { db.Close() })
	return db
}

// migrate applies the emitted outbox migration as the owner, in one transaction.
func (e *env) migrate() error {
	text, err := Migration(e.relayRole, e.runtimeRole, e.workerRole)
	if err != nil {
		e.t.Fatal(err)
	}
	_, err = e.owner.ExecContext(e.ctx, text)
	return err
}

// migrateAll applies every emitted migration of the service as the owner, each once: the key table of the
// idempotency component, the audit tables and their partitions, and the outbox queue.
func (e *env) migrateAll() {
	e.t.Helper()
	auditText, err := auditlog.Migration(e.runtimeRole)
	if err != nil {
		e.t.Fatal(err)
	}
	keysText, err := idempotency.SchemaSQL(e.purgeRole, e.runtimeRole)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, step := range []struct{ name, text string }{{"idempotency", keysText}, {"audit-log", auditText}} {
		if _, err := e.owner.ExecContext(e.ctx, step.text); err != nil {
			e.t.Fatalf("the %s migration: %v", step.name, err)
		}
	}
	if err := e.migrate(); err != nil {
		e.t.Fatalf("the outbox migration: %v", err)
	}
	for more := true; more; {
		if err := e.owner.QueryRowContext(e.ctx, "SELECT bool_or(step_left) FROM "+audit.MaintainFunction+"()").Scan(&more); err != nil {
			e.t.Fatalf("audit_maintain: %v", err)
		}
	}
}

// value reads one value of the statement as the owner.
func (e *env) value(statement string, args ...any) string {
	e.t.Helper()
	var v sql.NullString
	if err := e.owner.QueryRowContext(e.ctx, statement, args...).Scan(&v); err != nil {
		e.t.Fatalf("%s: %v", statement, err)
	}
	return v.String
}

// messages counts the messages of every tenant in the runtime queue table that match where. The owner does not
// inherit the outbox relay role, so it reads no message as itself; it takes the role with SET ROLE, as the
// migration does.
func (e *env) messages(where string) string {
	e.t.Helper()
	tx, err := e.owner.BeginTx(e.ctx, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(e.ctx, "SET LOCAL ROLE "+e.relayRole); err != nil {
		e.t.Fatal(err)
	}
	var n string
	if err := tx.QueryRowContext(e.ctx, "SELECT count(*) FROM stego_outbox.messages WHERE "+where).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// tn returns the tenant of the ID, as the tenant runtime builds it from a column that a statement read.
func (e *env) tn(id string) tenant.Tenant {
	e.t.Helper()
	rows, err := e.owner.QueryContext(e.ctx, "SELECT $1::text", id)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		e.t.Fatal("no row")
	}
	t, err := tenant.ScanRows(rows)
	if err != nil {
		e.t.Fatal(err)
	}
	return t
}

// formerMigration is the migration of the former outbox component, which the generator test copies here.
func formerMigration(t *testing.T) string {
	t.Helper()
	text, err := os.ReadFile("testdata/former.sql")
	if err != nil {
		t.Fatal(err)
	}
	return string(text)
}

// runtimeColumns are columns of the runtime queue table that the former table does not have.
var runtimeColumns = []string{"tenant_id", "state", "conflicts", "outgoing_key", "parked_from", "must_land", "idempotent"}

// queueShape fails the test unless stego_outbox.messages is the runtime queue table, with its policies and its
// relay functions.
func (e *env) queueShape(t *testing.T) {
	t.Helper()
	for _, column := range runtimeColumns {
		if e.value(`SELECT count(*) FROM information_schema.columns WHERE table_schema = 'stego_outbox'
			AND table_name = 'messages' AND column_name = $1`, column) != "1" {
			t.Fatalf("stego_outbox.messages has no column %s of the runtime queue table", column)
		}
	}
	if got := e.value(`SELECT string_agg(policyname, ',' ORDER BY policyname) FROM pg_policies
		WHERE schemaname = 'stego_outbox' AND tablename = 'messages'`); got != "messages_relay_delete,messages_relay_select,messages_relay_update,messages_tenant" {
		t.Fatalf("the policies of stego_outbox.messages are %s, want the tenant policy and the three relay policies", got)
	}
	if got := e.value(`SELECT string_agg(p.proname, ',' ORDER BY p.proname) FROM pg_proc p
		JOIN pg_namespace n ON n.oid = p.pronamespace JOIN pg_roles r ON r.oid = p.proowner
		WHERE n.nspname = 'stego_outbox' AND r.rolname = $1 AND p.prosecdef`, e.relayRole); got != "blocked,claim,move,oldest,purge_done,purge_parked,release,resolve" {
		t.Fatalf("the relay functions of stego_outbox are %s, want the eight functions of the runtime migration", got)
	}
}

func TestMigrations(t *testing.T) {
	t.Run("ST-SUP-RC18-OC30-T01 One migration creates the queue table", func(t *testing.T) {
		e := newEnv(t)
		e.migrateAll()
		e.queueShape(t)
		if got := e.value(`SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = 'stego_outbox' AND c.relkind = 'r'`); got != "1" {
			t.Fatalf("the schema stego_outbox holds %s tables, want the queue table only", got)
		}
		if _, err := Migration("same"+outbox.RelaySuffix, "same_role", "same_role"); err == nil {
			t.Fatal("the emitted migration took one role as the runtime role and the worker role")
		}
	})
	t.Run("ST-SUP-RC18-OC30-T02 An upgraded database gets the runtime queue table and keeps the former messages", func(t *testing.T) {
		e := newEnv(t)
		if _, err := e.owner.ExecContext(e.ctx, formerMigration(t)); err != nil {
			t.Fatalf("the former outbox migration: %v", err)
		}
		if _, err := e.owner.ExecContext(e.ctx, `INSERT INTO stego_outbox.messages (id, destination, resource_key, kind, payload)
			VALUES (gen_random_uuid(), 'kafka', 'user:1', 'user.created', '{"id":1}')`); err != nil {
			t.Fatal(err)
		}
		err := e.migrate()
		if err == nil || !strings.Contains(err.Error(), "holds messages of the former outbox component") {
			t.Fatalf("the migration over a former table that holds a message gave %v, want a refusal", err)
		}
		if e.value("SELECT count(*) FROM stego_outbox.messages") != "1" || e.value(`SELECT count(*) FROM information_schema.columns
			WHERE table_schema = 'stego_outbox' AND table_name = 'messages' AND column_name = 'tenant_id'`) != "0" {
			t.Fatal("the refused migration changed the former table or lost its message")
		}
		if _, err := e.owner.ExecContext(e.ctx, "DELETE FROM stego_outbox.messages"); err != nil {
			t.Fatal(err)
		}
		if err := e.migrate(); err != nil {
			t.Fatalf("the migration over an empty former table: %v", err)
		}
		e.queueShape(t)
		// The next generation applies the migration again, over the runtime queue table and its messages.
		t1 := e.tn("t1")
		queue, err := NewQueue(e.runtime, nil)
		if err != nil {
			t.Fatal(err)
		}
		tx, err := tenant.Begin(e.ctx, e.runtime, t1, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := queue.Enqueue(e.ctx, tx, outbox.Message{ID: uuid.New(), Destination: "audit-sink", ResourceKey: "k",
			Kind: "k", Payload: []byte(`{"a":1}`)}); err != nil {
			t.Fatal(err)
		}
		if err := tenant.Commit(e.ctx, tx, t1); err != nil {
			t.Fatal(err)
		}
		if err := e.migrate(); err != nil {
			t.Fatalf("the migration applied again: %v", err)
		}
		if e.messages("true") != "1" {
			t.Fatal("the migration applied again lost a message of the runtime queue table")
		}
	})
	t.Run("ST-SUP-RC18-OC30-T07 A writer of the former outbox that races the drop keeps its message", func(t *testing.T) {
		e := newEnv(t)
		if _, err := e.owner.ExecContext(e.ctx, formerMigration(t)); err != nil {
			t.Fatalf("the former outbox migration: %v", err)
		}
		// A writer of the former release inserts a message into the empty former table and has not committed when
		// the migration starts: the migration's check for messages cannot see the row yet.
		writer, err := e.owner.BeginTx(e.ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = writer.Rollback() }()
		if _, err := writer.ExecContext(e.ctx, `INSERT INTO stego_outbox.messages (id, destination, resource_key, kind, payload)
			VALUES (gen_random_uuid(), 'kafka', 'user:1', 'user.created', '{"id":1}')`); err != nil {
			t.Fatal(err)
		}
		migrated := make(chan error, 1)
		go func() { migrated <- e.migrate() }()
		// The migration waits for a lock on the former table before it drops it.
		deadline := time.Now().Add(15 * time.Second)
		for e.value(`SELECT count(*) FROM pg_catalog.pg_locks l JOIN pg_catalog.pg_class c ON c.oid = l.relation
			JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = 'stego_outbox' AND c.relname = 'messages' AND NOT l.granted`) == "0" {
			if time.Now().After(deadline) {
				t.Fatal("the migration did not wait for the writer of the former table")
			}
			time.Sleep(20 * time.Millisecond)
		}
		if err := writer.Commit(); err != nil {
			t.Fatal(err)
		}
		select {
		case err = <-migrated:
		case <-time.After(30 * time.Second):
			t.Fatal("the migration did not end after the writer committed")
		}
		if err == nil || !strings.Contains(err.Error(), "holds messages of the former outbox component") {
			t.Fatalf("the migration over the message of a racing former writer gave %v, want a refusal", err)
		}
		if e.value("SELECT count(*) FROM stego_outbox.messages") != "1" {
			t.Fatal("the migration lost the message of the racing former writer")
		}
	})
}

func TestAccess(t *testing.T) {
	t.Run("ST-SUP-RC18-OC30-T03 The roles get only the grants of the runtime migration and the tenant wall", func(t *testing.T) {
		e := newEnv(t)
		e.migrateAll()
		table := "stego_outbox.messages"
		for _, c := range []struct {
			role, check, privilege string
			want                   bool
		}{
			{e.runtimeRole, "table", "SELECT", true},
			{e.runtimeRole, "table", "INSERT", false},
			{e.runtimeRole, "table", "UPDATE", false},
			{e.runtimeRole, "table", "DELETE", false},
			{e.runtimeRole, "column tenant_id", "INSERT", true},
			{e.runtimeRole, "column state", "INSERT", false},
			{e.runtimeRole, "column attempts", "INSERT", false},
			{e.workerRole, "table", "UPDATE", false},
			{e.workerRole, "table", "DELETE", false},
		} {
			var statement string
			if column, ok := strings.CutPrefix(c.check, "column "); ok {
				statement = fmt.Sprintf("SELECT has_column_privilege('%s', '%s', '%s', '%s')", c.role, table, column, c.privilege)
			} else {
				statement = fmt.Sprintf("SELECT has_table_privilege('%s', '%s', '%s')", c.role, table, c.privilege)
			}
			if got := e.value(statement) == "true"; got != c.want {
				t.Fatalf("%s of %s on the %s: %t, want %t", c.privilege, c.role, c.check, got, c.want)
			}
		}
		claim := "stego_outbox.claim(timestamptz, integer, uuid, bigint)"
		if e.value("SELECT has_function_privilege($1, $2, 'EXECUTE')", e.runtimeRole, claim) != "false" ||
			e.value("SELECT has_function_privilege($1, $2, 'EXECUTE')", e.workerRole, claim) != "true" {
			t.Fatal("the claim function must run for the worker role only")
		}
		if e.value(`SELECT relrowsecurity AND relforcerowsecurity FROM pg_class WHERE oid = 'stego_outbox.messages'::regclass`) != "true" {
			t.Fatal("row-level security is not enabled and forced on stego_outbox.messages")
		}
		if got := e.value(`SELECT pg_get_indexdef('stego_outbox.messages_open_order'::regclass)`); !strings.Contains(got, "(tenant_id, destination, resource_key, sequence)") {
			t.Fatalf("the order index is %s, want the tenant, the destination and the resource key", got)
		}
		queue, err := NewQueue(e.runtime, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"t1", "t2"} {
			tn := e.tn(id)
			tx, err := tenant.Begin(e.ctx, e.runtime, tn, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := queue.Enqueue(e.ctx, tx, outbox.Message{ID: uuid.New(), Destination: "audit-sink", ResourceKey: "k",
				Kind: "k", Payload: []byte(`{"a":1}`)}); err != nil {
				t.Fatal(err)
			}
			if err := tenant.Commit(e.ctx, tx, tn); err != nil {
				t.Fatal(err)
			}
		}
		t1 := e.tn("t1")
		tx, err := tenant.Begin(e.ctx, e.runtime, t1, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		var seen string
		if err := tx.QueryRowContext(e.ctx, "SELECT string_agg(tenant_id, ',') FROM stego_outbox.messages").Scan(&seen); err != nil {
			t.Fatal(err)
		}
		if seen != "t1" {
			t.Fatalf("a transaction of t1 sees the messages of %s, want t1 only", seen)
		}
		if _, err := tx.ExecContext(e.ctx, "UPDATE stego_outbox.messages SET state = 'done'"); err == nil {
			t.Fatal("the runtime role changed the state of a message")
		}
	})
}

func TestIdentity(t *testing.T) {
	t.Run("ST-SUP-RC18-OC11-T01 A repeated message ID stops the application write", func(t *testing.T) {
		e := newEnv(t)
		e.migrateAll()
		queue, err := NewQueue(e.runtime, nil)
		if err != nil {
			t.Fatal(err)
		}
		t1 := e.tn("t1")
		held := outbox.Message{ID: uuid.New(), Destination: "audit-sink", ResourceKey: "k", Kind: "k", Payload: []byte(`{"a":1}`)}
		tx, err := tenant.Begin(e.ctx, e.runtime, t1, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := queue.Enqueue(e.ctx, tx, held); err != nil {
			t.Fatal(err)
		}
		if err := tenant.Commit(e.ctx, tx, t1); err != nil {
			t.Fatal(err)
		}
		// A second transaction writes a new message and then a message with the ID that the queue holds: the queue
		// refuses the repeated ID, and the transaction commits nothing, also not the new message.
		tx, err = tenant.Begin(e.ctx, e.runtime, t1, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		fresh := outbox.Message{ID: uuid.New(), Destination: "audit-sink", ResourceKey: "j", Kind: "k", Payload: []byte(`{"a":2}`)}
		if err := queue.Enqueue(e.ctx, tx, fresh); err != nil {
			t.Fatal(err)
		}
		repeated := held
		repeated.Payload = []byte(`{"a":3}`)
		enqueueErr := queue.Enqueue(e.ctx, tx, repeated)
		commitErr := tenant.Commit(e.ctx, tx, t1)
		if enqueueErr == nil || commitErr == nil {
			t.Fatalf("the repeated message ID gave %v at enqueue and %v at commit, want both refused", enqueueErr, commitErr)
		}
		if got := e.messages("true"); got != "1" {
			t.Fatalf("the queue holds %s messages, want the first one only", got)
		}
		if got := e.messages(fmt.Sprintf("id = '%s' AND payload = convert_to('{\"a\":1}', 'UTF8')", held.ID)); got != "1" {
			t.Fatal("the repeated message ID changed the message that the queue holds")
		}
	})
}

// recordingSink is the audit sink that the audit-sink port binds in the test: it keeps each event it gets.
type recordingSink struct {
	mu     sync.Mutex
	events []audit.Event
}

func (s *recordingSink) Append(_ context.Context, events []audit.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range events {
		for _, held := range s.events {
			if held.ID == e.ID {
				return audit.ErrDuplicate
			}
		}
	}
	s.events = append(s.events, events...)
	return nil
}

func (s *recordingSink) Anchor(context.Context, audit.Anchor) error { return nil }

func (s *recordingSink) got() []audit.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]audit.Event(nil), s.events...)
}

func TestDelivery(t *testing.T) {
	t.Run("ST-SUP-RC18-OC30-T04 A message reaches the outside system through the destination that the wiring registers", func(t *testing.T) {
		e := newEnv(t)
		e.migrateAll()
		if got := strings.Join(Destinations, ","); got != audit.SinkDestination {
			t.Fatalf("the destinations of the service are %q, want %s", got, audit.SinkDestination)
		}
		if limit := PayloadLimits()[audit.SinkDestination]; limit != audit.MaxSinkMessage {
			t.Fatalf("the payload limit of %s is %d, want %d", audit.SinkDestination, limit, audit.MaxSinkMessage)
		}
		queue, err := NewQueue(e.runtime, nil)
		if err != nil {
			t.Fatal(err)
		}
		if queue.PayloadLimit(audit.SinkDestination) != audit.MaxSinkMessage || queue.PayloadLimit("other") != outbox.MaxPayloadBytes {
			t.Fatal("the queue does not take the payload limit of each destination")
		}
		enqueuer, err := AuditQueue(queue)
		if err != nil {
			t.Fatal(err)
		}
		writer, err := audit.NewWriter(enqueuer, nil)
		if err != nil {
			t.Fatal(err)
		}
		t1 := e.tn("t1")
		tx, err := tenant.Begin(e.ctx, e.runtime, t1, nil)
		if err != nil {
			t.Fatal(err)
		}
		stored, err := writer.Record(e.ctx, tx, t1, []audit.Event{{TenantID: "t1", ActorID: "user:u-7",
			ActorKind: audit.ActorUser, Action: "create", ResourceType: "Order", ResourceID: "o1",
			Outcome: audit.OutcomeOK, RequestID: "r1", Service: "orders"}})
		if err != nil {
			t.Fatal(err)
		}
		if err := tenant.Commit(e.ctx, tx, t1); err != nil {
			t.Fatal(err)
		}

		workerQueue, err := NewQueue(e.worker, nil)
		if err != nil {
			t.Fatal(err)
		}
		config := outbox.DefaultWorkerConfig()
		config.PollInterval = 20 * time.Millisecond
		if _, err := NewWorker(workerQueue, config, Ports{}); err == nil || !strings.Contains(err.Error(), "audit-sink port") {
			t.Fatalf("the worker with no audit-sink port gave %v, want a refusal that names the port", err)
		}
		sink := &recordingSink{}
		worker, err := NewWorker(workerQueue, config, Ports{AuditSink: sink})
		if err != nil {
			t.Fatal(err)
		}
		registrations := worker.Registrations()
		if len(registrations) != 1 || registrations[0].Name != audit.SinkDestination || !registrations[0].Idempotent || !registrations[0].MustLand {
			t.Fatalf("the worker holds %+v, want audit-sink, idempotent and must land", registrations)
		}
		runCtx, cancel := context.WithCancel(e.ctx)
		done := make(chan error, 1)
		go func() { done <- worker.Run(runCtx) }()
		deadline := time.Now().Add(15 * time.Second)
		for (len(sink.got()) == 0 || e.messages("state = 'done'") != "1") && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("the worker ended with %v", err)
		}
		got := sink.got()
		if len(got) != 1 || got[0].ID != stored[0].ID || got[0].TenantID != "t1" {
			t.Fatalf("the sink got %+v, want the one event of t1; the worker %+v", got, worker.Stats())
		}
		if e.messages("state = 'done'") != "1" {
			t.Fatal("the delivered message is not done")
		}
	})
}
