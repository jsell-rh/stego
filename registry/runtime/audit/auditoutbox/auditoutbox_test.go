package auditoutbox

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
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"stego.dev/runtime/audit"
	"stego.dev/runtime/internal/pgtest"
	"stego.dev/runtime/internal/seal"
	"stego.dev/runtime/outbox"
	"stego.dev/runtime/tenant"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

// recordingSink is an audit sink that keeps what it gets.
type recordingSink struct {
	mu      sync.Mutex
	events  []audit.Event
	anchors []audit.Anchor
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

func (s *recordingSink) Anchor(_ context.Context, a audit.Anchor) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.anchors = append(s.anchors, a)
	return nil
}

func (s *recordingSink) got() []audit.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]audit.Event(nil), s.events...)
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

const rolePassword = "auditoutbox-test"

// env is one database of the local PostgreSQL server of pgtest with the outbox runtime's queue and the audit
// runtime's tables, a runtime role and an outbox worker role that are not superusers.
type env struct {
	admin    *sql.DB
	runtime  *sql.DB
	workerDB *sql.DB
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
	roles := [2]string{"ao_rt_" + tag, "ao_worker_" + tag}
	for _, role := range roles {
		if _, err := root.ExecContext(ctx, "CREATE ROLE "+role+" LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD '"+rolePassword+"'"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = root.ExecContext(context.Background(), "DROP ROLE IF EXISTS "+role) })
	}
	database := pgtest.Database(t, "auditoutbox")
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
	queueDDL, err := outbox.Migration(roles[0], roles[1])
	if err != nil {
		t.Fatal(err)
	}
	auditDDL, err := audit.DDL(audit.Config{RuntimeRole: roles[0], AnchorPeriod: time.Hour, RetentionMonths: 12})
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
	open := func(role string) *sql.DB {
		u, err := url.Parse(database)
		if err != nil {
			t.Fatal(err)
		}
		u.User = url.UserPassword(role, rolePassword)
		db, err := sql.Open("pgx", u.String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	return &env{admin: admin, runtime: open(roles[0]), workerDB: open(roles[1])}
}

func (e *env) count(t *testing.T, statement string) int {
	t.Helper()
	var n int
	if err := e.admin.QueryRowContext(context.Background(), statement).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSinkThroughOutbox(t *testing.T) {
	t.Run("ST-SUP-RP05-LC14-T01 An audit row reaches the sink through the runtime adapter", func(t *testing.T) {
		e := newEnv(t)
		ctx := context.Background()
		queue, err := outbox.New(e.runtime, outbox.Options{PayloadLimits: PayloadLimits()})
		if err != nil {
			t.Fatal(err)
		}
		adapter, err := NewQueue(queue)
		if err != nil {
			t.Fatal(err)
		}
		writer, err := audit.NewWriter(adapter, nil)
		if err != nil {
			t.Fatal(err)
		}
		t1 := mustTenant(t, "t1")
		record := func(requestID string) (*sql.Tx, []audit.Event) {
			tx, err := tenant.Begin(ctx, e.runtime, t1, nil)
			if err != nil {
				t.Fatal(err)
			}
			stored, err := writer.Record(ctx, tx, t1, []audit.Event{{TenantID: "t1", ActorID: "user:u-7",
				ActorKind: audit.ActorUser, Action: "create", ResourceType: "order", ResourceID: "o1",
				Outcome: audit.OutcomeOK, RequestID: requestID, Service: "orders"}})
			if err != nil {
				t.Fatal(err)
			}
			return tx, stored
		}

		tx, _ := record("r-rolled-back")
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
		if n := e.count(t, "SELECT count(*) FROM stego_outbox.messages"); n != 0 {
			t.Fatalf("a rolled back Record left %d outbox rows", n)
		}
		tx, stored := record("r-committed")
		if err := tenant.Commit(ctx, tx, t1); err != nil {
			t.Fatal(err)
		}
		var id, destination, tenantID string
		var idempotent, mustLand bool
		var xminRow, xminEvent string
		if err := e.admin.QueryRowContext(ctx, `SELECT id::text, destination, tenant_id, idempotent, must_land, xmin::text
FROM stego_outbox.messages`).Scan(&id, &destination, &tenantID, &idempotent, &mustLand, &xminRow); err != nil {
			t.Fatalf("one outbox row after the commit: %v", err)
		}
		if err := e.admin.QueryRowContext(ctx, "SELECT xmin::text FROM "+audit.EventsTable+" WHERE id = $1",
			stored[0].ID.String()).Scan(&xminEvent); err != nil {
			t.Fatal(err)
		}
		if id != stored[0].ID.String() || destination != audit.SinkDestination || tenantID != "t1" || !idempotent || !mustLand {
			t.Fatalf("the outbox row is %s %s %s idempotent %t must land %t, want the event's ID for audit-sink of t1",
				id, destination, tenantID, idempotent, mustLand)
		}
		if xminRow != xminEvent {
			t.Fatalf("the outbox row was written in transaction %s and the audit row in %s, want the same", xminRow, xminEvent)
		}

		// The outbox keeps the exact canonical bytes of the sink message.
		var payload []byte
		if err := e.admin.QueryRowContext(ctx, `SELECT payload FROM stego_outbox.messages`).Scan(&payload); err != nil {
			t.Fatal(err)
		}
		if string(payload) != string(sinkPayload(t, stored[0])) {
			t.Fatalf("the outbox holds\n%s\nwant the canonical sink message\n%s", payload, sinkPayload(t, stored[0]))
		}
		sink := &recordingSink{}
		auditDestination, err := audit.NewDestination(sink)
		if err != nil {
			t.Fatal(err)
		}
		s, err := NewSink(auditDestination)
		if err != nil {
			t.Fatal(err)
		}
		reg := s.Destination()
		if reg.Name != audit.SinkDestination || !reg.Idempotent || !reg.MustLand {
			t.Fatalf("the registration is %+v, want audit-sink, idempotent and must land", reg)
		}
		workerQueue, err := outbox.New(e.workerDB, outbox.Options{})
		if err != nil {
			t.Fatal(err)
		}
		config := outbox.DefaultWorkerConfig()
		config.PollInterval = 20 * time.Millisecond
		worker, err := outbox.NewWorker(workerQueue, config)
		if err != nil {
			t.Fatal(err)
		}
		if err := worker.Register(reg); err != nil {
			t.Fatal(err)
		}
		runCtx, cancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- worker.Run(runCtx) }()
		deadline := time.Now().Add(10 * time.Second)
		for len(sink.got()) == 0 && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		for e.count(t, "SELECT count(*) FROM stego_outbox.messages WHERE state = 'done'") == 0 && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("the worker ended with %v", err)
		}
		got := sink.got()
		if len(got) != 1 {
			var state, code string
			_ = e.admin.QueryRowContext(ctx, "SELECT state, failure_code FROM stego_outbox.messages").Scan(&state, &code)
			t.Fatalf("the sink got %d events, want one; the worker %+v, the row %s %s", len(got), worker.Stats(), state, code)
		}
		want, err := audit.Encode(stored[0])
		if err != nil {
			t.Fatal(err)
		}
		gotForm, err := audit.Encode(got[0])
		if err != nil {
			t.Fatal(err)
		}
		hash, err := audit.Hash(got[0])
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(gotForm, want) || !bytes.Equal(got[0].Hash, stored[0].Hash) || !bytes.Equal(hash, stored[0].Hash) {
			t.Fatalf("the sink got\n%s\nwant the row's canonical form and hash\n%s", gotForm, want)
		}
		if n := e.count(t, "SELECT count(*) FROM stego_outbox.messages WHERE state = 'done'"); n != 1 {
			t.Fatalf("%d outbox rows are done, want one", n)
		}

		// A message whose event names another tenant than its outbox row is parked, never given to the sink.
		other := got[0]
		other.TenantID = "t2"
		if err := s.Deliver(ctx, nil, outbox.Delivery{Tenant: t1, Message: outbox.Message{Payload: sinkPayload(t, other)}}); !outbox.IsPermanent(err) {
			t.Fatalf("a message of another tenant gave %v, want a permanent refusal", err)
		}
		if err := s.Deliver(ctx, nil, outbox.Delivery{Tenant: t1, Message: outbox.Message{Payload: []byte(`{"kind":"event"}`)}}); !outbox.IsPermanent(err) {
			t.Fatalf("a message that does not decode gave %v, want a permanent refusal", err)
		}
		// The bytes are delivered as they were stored: a sink message that is not in its canonical form, byte for
		// byte, is refused for good and never rewritten.
		respaced := []byte(strings.Replace(string(sinkPayload(t, got[0])), `{"event":`, `{ "event":`, 1))
		if err := s.Deliver(ctx, nil, outbox.Delivery{Tenant: t1, Message: outbox.Message{Payload: respaced}}); !outbox.IsPermanent(err) {
			t.Fatalf("a sink message out of its canonical form gave %v, want a permanent refusal", err)
		}
		if len(sink.got()) != 1 {
			t.Fatal("a refused message reached the sink")
		}
	})
}

// sinkPayload is the canonical sink message of e, with its hash computed again so only its tenant differs.
func sinkPayload(t *testing.T, e audit.Event) []byte {
	t.Helper()
	h, err := audit.Hash(e)
	if err != nil {
		t.Fatal(err)
	}
	e.Hash = h
	rec, err := audit.Encode(e)
	if err != nil {
		t.Fatal(err)
	}
	return append(append([]byte(`{"event":`), rec...), []byte(`,"kind":"event"}`)...)
}

// bigEvent is an audit event of t1 whose after state holds a note of size bytes.
func bigEvent(t *testing.T, requestID string, size int) audit.Event {
	t.Helper()
	e := audit.Event{TenantID: "t1", ActorID: "user:u-7", ActorKind: audit.ActorUser, Action: "update",
		ResourceType: "order", ResourceID: "o1", Outcome: audit.OutcomeOK, RequestID: requestID, Service: "orders"}
	if err := e.SetStates(audit.Settings{Level: audit.LevelChange}, audit.Fields{}, map[string]any{"note": ""},
		map[string]any{"note": strings.Repeat("x", size)}); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestSinkPayloadLimit(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	t1 := mustTenant(t, "t1")
	queue, err := outbox.New(e.runtime, outbox.Options{PayloadLimits: PayloadLimits()})
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := NewQueue(queue)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := audit.NewWriter(adapter, nil)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("ST-SUP-RP05-LC14-T02 An audit event above 64 KiB reaches the outbox of audit-sink", func(t *testing.T) {
		tx, err := tenant.Begin(ctx, e.runtime, t1, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Record(ctx, tx, t1, []audit.Event{bigEvent(t, "r-big", 200<<10)}); err != nil {
			t.Fatalf("Record of an event of 200 KiB: %v", err)
		}
		if err := tenant.Commit(ctx, tx, t1); err != nil {
			t.Fatal(err)
		}
		if n := e.count(t, "SELECT count(*) FROM stego_outbox.messages WHERE destination = 'audit-sink' AND octet_length(payload) > 200*1024"); n != 1 {
			t.Fatalf("%d audit-sink messages above 200 KiB, want one", n)
		}
		// Another destination keeps the limit of 64 KiB.
		tx, err = tenant.Begin(ctx, e.runtime, t1, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		payload := []byte(`{"note":"` + strings.Repeat("x", 70<<10) + `"}`)
		if err := queue.Enqueue(ctx, tx, outbox.Message{ID: uuid.New(), Destination: "billing", ResourceKey: "charge:1",
			Kind: "charge", Payload: payload}); err == nil {
			t.Fatal("a billing message of 70 KiB was taken")
		}
		// A queue without the limit of audit-sink is refused at start.
		plain, err := outbox.New(e.runtime, outbox.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := NewQueue(plain); err == nil {
			t.Fatal("the adapter took a queue whose audit-sink limit is 64 KiB")
		}
		if _, err := outbox.New(e.runtime, outbox.Options{PayloadLimits: map[string]int{"audit-sink": 2 << 20}}); err == nil {
			t.Fatal("a payload limit of 2 MiB was taken")
		}
	})

	t.Run("ST-SUP-RP05-LC14-T03 An audit event above 1 MiB is refused before any write", func(t *testing.T) {
		before := e.count(t, "SELECT count(*) FROM stego_outbox.messages")
		tx, err := tenant.Begin(ctx, e.runtime, t1, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err := writer.Record(ctx, tx, t1, []audit.Event{bigEvent(t, "r-small", 10), bigEvent(t, "r-huge", 1<<20)}); !errors.Is(err, audit.ErrTooLarge) {
			t.Fatalf("Record of an event of 1 MiB gave %v, want ErrTooLarge", err)
		}
		if strings.Contains(audit.ErrTooLarge.Error(), "xxx") {
			t.Fatal("the error holds a value of the event")
		}
		// No statement was sent: the transaction is still good, and it commits with no audit row and no message.
		if _, err := tx.ExecContext(ctx, "SELECT 1"); err != nil {
			t.Fatalf("the transaction failed after the refusal: %v", err)
		}
		if err := tenant.Commit(ctx, tx, t1); err != nil {
			t.Fatal(err)
		}
		if n := e.count(t, "SELECT count(*) FROM "+audit.EventsTable+" WHERE request_id IN ('r-small', 'r-huge')"); n != 0 {
			t.Fatalf("%d audit rows of the refused batch", n)
		}
		if n := e.count(t, "SELECT count(*) FROM stego_outbox.messages"); n != before {
			t.Fatal("the refused batch enqueued a message")
		}
	})
}
