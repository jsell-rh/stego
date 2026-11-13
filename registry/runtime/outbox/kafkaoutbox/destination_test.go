package kafkaoutbox

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/jsell-rh/stego/registry/runtime/internal/pgtest"
	"github.com/jsell-rh/stego/registry/runtime/internal/seal"
	"github.com/jsell-rh/stego/registry/runtime/kafkatest"
	"github.com/jsell-rh/stego/registry/runtime/outbox"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
)

func TestMain(m *testing.M) { os.Exit(kafkatest.Wrap(func() int { return pgtest.Main(m) })) }

type decision struct{ seal.DecisionMark }

func mustTenant(t testing.TB, id string) tenant.Tenant {
	t.Helper()
	tn, err := tenant.New(tenant.Requested(tenant.ProvisionAction, id, decision{seal.NewDecisionMark(id, "provision")}))
	if err != nil {
		t.Fatal(err)
	}
	return tn
}

const rolePassword = "kafkaoutbox-test"

// env is one database of the local PostgreSQL server of pgtest with the outbox queue, a runtime role and an outbox
// worker role that are not superusers, and the local Kafka broker of kafkatest.
type env struct {
	admin   *sql.DB
	runtime *sql.DB       // the runtime role
	queue   *outbox.Queue // of the runtime role
	wqueue  *outbox.Queue // of the worker role
	broker  *kafkatest.Broker
}

func newEnv(t *testing.T) *env {
	t.Helper()
	broker := kafkatest.Start(t)
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
	roles := [2]string{"ko_rt_" + tag, "ko_worker_" + tag}
	for _, role := range roles {
		if _, err := root.ExecContext(ctx, "CREATE ROLE "+role+" LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD '"+rolePassword+"'"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = root.ExecContext(context.Background(), "DROP ROLE IF EXISTS "+role) })
	}
	// The outbox migration makes the outbox relay role of the test; it is dropped after the database.
	outboxRelay := "ko_" + tag + outbox.RelaySuffix
	t.Cleanup(func() { _, _ = root.ExecContext(context.Background(), "DROP ROLE IF EXISTS "+outboxRelay) })
	database := pgtest.Database(t, "kafkaoutbox")
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
	ddl, err := outbox.Migration(outboxRelay, roles[0], roles[1])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(ctx, ddl); err != nil {
		t.Fatal(err)
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
	e := &env{admin: admin, runtime: open(roles[0]), broker: broker}
	if e.queue, err = outbox.New(e.runtime, outbox.Options{}); err != nil {
		t.Fatal(err)
	}
	if e.wqueue, err = outbox.New(open(roles[1]), outbox.Options{}); err != nil {
		t.Fatal(err)
	}
	return e
}

// plain gives the settings of the plaintext listener through its proxy, with topic as the declared topic.
func (e *env) plain(topic string) Settings {
	return Settings{Brokers: []string{e.broker.Plain.Addr()}, Topic: topic, InsecureLoopback: true}
}

// destination builds the destination of s and closes it when the test ends.
func destination(t *testing.T, s Settings) *Destination {
	t.Helper()
	d, err := New(s, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	return d
}

// work runs an outbox worker of the worker role with the destination d until the test t ends.
func (e *env) work(t *testing.T, d *Destination, change func(*outbox.WorkerConfig)) {
	t.Helper()
	config := outbox.DefaultWorkerConfig()
	config.Concurrency, config.PollInterval = 4, 20*time.Millisecond
	config.AttemptTimeout, config.LeaseDuration = 3*time.Second, 15*time.Second
	config.RetryMin, config.RetryMax = 100*time.Millisecond, 400*time.Millisecond
	if change != nil {
		change(&config)
	}
	worker, err := outbox.NewWorker(e.wqueue, config)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Register(d.Registration()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = worker.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
}

// event enqueues one event of the type kind for the entity key with payload in its own transaction of tenantID, as a
// command stages it, and gives its message ID.
func (e *env) event(t *testing.T, tenantID, kind, key string, payload any) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	tn := mustTenant(t, tenantID)
	tx, err := tenant.Begin(ctx, e.runtime, tn, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	id := uuid.New()
	if err := e.queue.Enqueue(ctx, tx, outbox.Message{ID: id, Destination: DestinationName, ResourceKey: key, Kind: kind,
		Payload: body, OutgoingKey: "key-" + id.String(), Idempotent: true}); err != nil {
		t.Fatal(err)
	}
	if err := tenant.Commit(ctx, tx, tn); err != nil {
		t.Fatal(err)
	}
	return id
}

// row is the dispatch state of one message.
type row struct {
	state, failure string
	attempts       int64
}

func (e *env) row(t *testing.T, id uuid.UUID) row {
	t.Helper()
	var r row
	if err := e.admin.QueryRowContext(context.Background(),
		`SELECT state, failure_code, attempts FROM stego_outbox.messages WHERE id = $1`, id).Scan(&r.state, &r.failure, &r.attempts); err != nil {
		t.Fatal(err)
	}
	return r
}

// until waits at most limit for the message id to reach a row that ok takes, and gives it.
func (e *env) until(t *testing.T, id uuid.UUID, limit time.Duration, ok func(row) bool) row {
	t.Helper()
	end := time.Now().Add(limit)
	for {
		r := e.row(t, id)
		if ok(r) {
			return r
		}
		if time.Now().After(end) {
			t.Fatalf("the message %s is %+v after %v", id, r, limit)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func done(r row) bool { return r.state == string(outbox.Done) }

// topicName is a new topic name for one test.
func topicName(prefix string) string {
	return prefix + "-" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
}

func header(t *testing.T, r *kgo.Record, key string) string {
	t.Helper()
	v, ok := kafkatest.Header(r, key)
	if !ok {
		t.Fatalf("the record at offset %d has not one header %s", r.Offset, key)
	}
	return v
}

func TestDelivery(t *testing.T) {
	e := newEnv(t)

	t.Run("ST-SUP-RP01-LC30-T02 One entity's events arrive in their order, on one partition, keyed by tenant and entity", func(t *testing.T) {
		topic := topicName("orders")
		e.broker.CreateTopic(t, topic, 3)
		e.work(t, destination(t, e.plain(topic)), nil)
		// Two tenants with the same two entity IDs: four entities, four keys.
		keys := []string{"t1/order:o-1", "t1/order:o-2", "t2/order:o-1", "t2/order:o-2"}
		const each = 8
		for n := range each {
			for _, key := range keys {
				tenantID, entity, _ := strings.Cut(key, "/")
				e.event(t, tenantID, "order.changed", entity, map[string]any{"key": key, "n": n})
			}
		}
		records := e.broker.Records(t, topic, each*len(keys), time.Minute, 2*time.Second)
		if len(records) != each*len(keys) {
			t.Fatalf("%d records, want %d", len(records), each*len(keys))
		}
		next := map[string]int{}
		partition := map[string]int32{}
		partitions := map[int32]bool{}
		for _, r := range records {
			var body struct {
				Key string
				N   int
			}
			if err := json.Unmarshal(r.Value, &body); err != nil {
				t.Fatal(err)
			}
			tenantID, entity, _ := strings.Cut(body.Key, "/")
			if string(r.Key) != tenantID+"\x1f"+entity || header(t, r, HeaderTenant) != tenantID {
				t.Fatalf("the record key is %q, want the tenant %s, the separator and the entity %s", r.Key, tenantID, entity)
			}
			if p, seen := partition[body.Key]; seen && p != r.Partition {
				t.Fatalf("%s is on the partitions %d and %d", body.Key, p, r.Partition)
			}
			partition[body.Key] = r.Partition
			partitions[r.Partition] = true
			if body.N != next[body.Key] {
				t.Fatalf("%s: event %d came where %d was due", body.Key, body.N, next[body.Key])
			}
			next[body.Key]++
		}
		if len(partitions) < 2 {
			t.Logf("the four keys share one partition of three")
		}
	})

	t.Run("ST-SUP-RP01-LC30-T03 An event goes to the declared topic, or else to the topic of its type", func(t *testing.T) {
		created, deleted, declared := topicName("user.created"), topicName("user.deleted"), topicName("users")
		for _, topic := range []string{created, deleted, declared} {
			e.broker.CreateTopic(t, topic, 1)
		}
		byType := destination(t, e.plain(""))
		toDeclared := destination(t, e.plain(declared))
		tn := mustTenant(t, "t1")
		ctx := context.Background()
		send := func(d *Destination, kind string) {
			t.Helper()
			id := uuid.New()
			delivery := outbox.Delivery{Tenant: tn, Message: outbox.Message{ID: id, Destination: DestinationName,
				ResourceKey: "user:u-1", Kind: kind, Payload: json.RawMessage(`{"id":"u-1"}`), OutgoingKey: id.String()}}
			attempt, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			if err := d.Deliver(attempt, nil, delivery); err != nil {
				t.Fatalf("deliver %s: %v", kind, err)
			}
		}
		send(byType, created)
		send(byType, deleted)
		send(toDeclared, created)
		send(toDeclared, deleted)
		for _, topic := range []string{created, deleted} {
			records := e.broker.Records(t, topic, 1, 30*time.Second, time.Second)
			if len(records) != 1 || header(t, records[0], HeaderEventType) != topic {
				t.Fatalf("the topic %s holds %d records, want its one event", topic, len(records))
			}
		}
		records := e.broker.Records(t, declared, 2, 30*time.Second, time.Second)
		if len(records) != 2 || header(t, records[0], HeaderEventType) != created || header(t, records[1], HeaderEventType) != deleted {
			t.Fatalf("the declared topic holds %d records, want both events", len(records))
		}
	})

	t.Run("ST-SUP-RP01-LC31-T01 A broker outage keeps the message in the outbox, and it arrives after", func(t *testing.T) {
		topic := topicName("outage")
		e.broker.CreateTopic(t, topic, 1)
		e.broker.Plain.Down()
		defer e.broker.Plain.Up()
		e.work(t, destination(t, e.plain(topic)), nil)
		id := e.event(t, "t1", "order.changed", "order:o-9", map[string]any{"n": 1})
		r := e.until(t, id, 30*time.Second, func(r row) bool {
			return r.attempts >= 3 && r.state == string(outbox.Pending) || r.state == string(outbox.Done) || r.state == string(outbox.Parked)
		})
		if r.state != string(outbox.Pending) {
			t.Fatalf("during the outage the message is %+v, want it waiting", r)
		}
		if r.failure != "delivery-not-sent" {
			t.Fatalf("during the outage the failure code is %q, want delivery-not-sent", r.failure)
		}
		if records := e.broker.Records(t, topic, 0, time.Second, time.Second); len(records) != 0 {
			t.Fatalf("the topic holds %d records during the outage", len(records))
		}
		e.broker.Plain.Up()
		e.until(t, id, 30*time.Second, done)
		records := e.broker.Records(t, topic, 1, 30*time.Second, 2*time.Second)
		if len(records) != 1 || header(t, records[0], HeaderEventID) != id.String() {
			t.Fatalf("after the outage the topic holds %d records, want the one event", len(records))
		}
	})

	t.Run("ST-SUP-RP01-LC31-T02 A send again after a lost answer carries the same event ID", func(t *testing.T) {
		topic := topicName("resend")
		e.broker.CreateTopic(t, topic, 1)
		e.work(t, destination(t, e.plain(topic)), nil)
		before := e.broker.Plain.Produced()
		e.broker.Plain.LoseNextAnswer()
		defer e.broker.Plain.Up()
		id := e.event(t, "t1", "order.changed", "order:o-7", map[string]any{"n": 1})
		// The broker takes the record and its answer is lost; the attempt ends with its outcome not known.
		deadline := time.Now().Add(30 * time.Second)
		for e.broker.Plain.Produced() == before {
			if time.Now().After(deadline) {
				t.Fatal("no produce request reached the broker")
			}
			time.Sleep(20 * time.Millisecond)
		}
		e.until(t, id, 30*time.Second, func(r row) bool { return r.attempts >= 2 })
		e.broker.Plain.Up()
		r := e.until(t, id, 30*time.Second, done)
		records := e.broker.Records(t, topic, 2, 30*time.Second, 2*time.Second)
		if len(records) != 2 {
			t.Fatalf("the topic holds %d records, want the first send and the send again", len(records))
		}
		for _, record := range records {
			if header(t, record, HeaderEventID) != id.String() || header(t, record, HeaderIdempotencyKey) != "key-"+id.String() ||
				string(record.Key) != "t1\x1forder:o-7" {
				t.Fatalf("a record carries the event ID %q and key %q, want %s and order:o-7",
					header(t, record, HeaderEventID), record.Key, id)
			}
		}
		if r.attempts < 2 {
			t.Fatalf("the message took %d attempts, want at least 2", r.attempts)
		}
	})

	t.Run("ST-SUP-RP01-LC31-T04 An attempt ends at its deadline when the broker holds its answer", func(t *testing.T) {
		topic := topicName("held")
		e.broker.CreateTopic(t, topic, 1)
		d := destination(t, e.plain(topic))
		e.broker.Plain.Hold()
		defer e.broker.Plain.Up()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		id := uuid.New()
		start := time.Now()
		err := d.Deliver(ctx, nil, outbox.Delivery{Tenant: mustTenant(t, "t1"), Message: outbox.Message{ID: id,
			Destination: DestinationName, ResourceKey: "order:o-3", Kind: "order.changed", Payload: json.RawMessage(`{}`),
			OutgoingKey: id.String()}})
		took := time.Since(start)
		if err == nil || outbox.IsPermanent(err) {
			t.Fatalf("a send whose answer the broker holds gave %v, want an outcome that is not known", err)
		}
		if took > 3*time.Second {
			t.Fatalf("the attempt took %v, past its deadline of 2s", took)
		}
	})

	t.Run("ST-SUP-RP01-LC31-T03 A message is parked when its declared attempts are exhausted", func(t *testing.T) {
		e.broker.Plain.Down()
		defer e.broker.Plain.Up()
		e.work(t, destination(t, e.plain(topicName("parked"))), func(c *outbox.WorkerConfig) { c.MaxAttempts = 3 })
		id := e.event(t, "t1", "order.changed", "order:o-5", map[string]any{"n": 1})
		r := e.until(t, id, 30*time.Second, func(r row) bool { return r.state == string(outbox.Parked) })
		if r.attempts != 3 || r.failure != "attempts-exhausted" {
			t.Fatalf("the parked message is %+v, want 3 attempts and attempts-exhausted", r)
		}
	})
}

func TestSecurity(t *testing.T) {
	broker := kafkatest.Start(t)
	tn := mustTenant(t, "t1")
	topic := topicName("secure")
	broker.CreateTopic(t, topic, 1)
	sent := 0
	send := func(t *testing.T, d *Destination) error {
		t.Helper()
		id := uuid.New()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := d.Deliver(ctx, nil, outbox.Delivery{Tenant: tn, Message: outbox.Message{ID: id, Destination: DestinationName,
			ResourceKey: "user:u-1", Kind: "user.changed", Payload: json.RawMessage(`{"id":"u-1"}`), OutgoingKey: id.String()}})
		if err == nil {
			sent++
		}
		return err
	}
	expect := func(t *testing.T) {
		t.Helper()
		if records := broker.Records(t, topic, sent, 30*time.Second, time.Second); len(records) != sent {
			t.Fatalf("the topic holds %d records, want the %d sent", len(records), sent)
		}
	}
	write := func(t *testing.T, name, content string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	t.Run("ST-SUP-RP01-LC32-T02 TLS verifies the broker against the CA file and sends the client certificate", func(t *testing.T) {
		good := destination(t, Settings{Brokers: []string{broker.TLS}, Topic: topic, CAFile: broker.CAFile,
			ClientCertFile: broker.ClientCertFile, ClientKeyFile: broker.ClientKeyFile})
		if err := send(t, good); err != nil {
			t.Fatalf("a send with the CA and the client certificate: %v", err)
		}
		logs := &recorder{}
		otherCA, err := New(Settings{Brokers: []string{broker.TLS}, Topic: topic, CAFile: broker.OtherCAFile,
			ClientCertFile: broker.ClientCertFile, ClientKeyFile: broker.ClientKeyFile}, logs)
		if err != nil {
			t.Fatal(err)
		}
		defer otherCA.Close()
		if err := send(t, otherCA); err == nil || err.Error() != errTLSVerify.Error() {
			t.Fatalf("a broker whose certificate the CA file did not sign gave %v, want %q", err, errTLSVerify)
		}
		if got := logs.got(); len(got) == 0 || got[0] != FailureTLSVerification {
			t.Fatalf("logged %v, want %s", got, FailureTLSVerification)
		}
		// The SASL listener takes TLS with no client certificate; with SCRAM below. Here the TLS listener requires one.
		noCert := destination(t, Settings{Brokers: []string{broker.TLS}, Topic: topic, CAFile: broker.CAFile,
			Mechanism: MechanismOAuthBearer, TokenFile: write(t, "token", kafkatest.Token("svc", time.Now().Add(time.Hour)))})
		if err := send(t, noCert); err == nil {
			t.Fatal("the listener that requires a client certificate took a send with none")
		}
		if _, err := New(Settings{Brokers: []string{broker.TLS}, CAFile: write(t, "ca", "not a certificate"),
			ClientCertFile: broker.ClientCertFile, ClientKeyFile: broker.ClientKeyFile}, nil); err == nil ||
			!strings.Contains(err.Error(), CAFileSetting) {
			t.Fatalf("a CA file with no certificate gave %v, want a refusal that names %s", err, CAFileSetting)
		}
		expect(t)
	})

	t.Run("ST-SUP-RP01-LC32-T03 SCRAM-SHA-512 reads the password file again at each authentication", func(t *testing.T) {
		broker.SetSCRAM(t, "svc-orders", "right-password")
		password := write(t, "password", "wrong-password\n")
		logs := &recorder{}
		d, err := New(Settings{Brokers: []string{broker.SASL}, Topic: topic, CAFile: broker.CAFile,
			Mechanism: MechanismSCRAMSHA512, Username: "svc-orders", PasswordFile: password}, logs)
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		if err := send(t, d); err == nil || err.Error() != errSASL.Error() {
			t.Fatalf("a wrong password gave %v, want %q", err, errSASL)
		}
		if got := logs.got(); len(got) == 0 || got[0] != FailureSASL {
			t.Fatalf("logged %v, want %s", got, FailureSASL)
		}
		if err := os.WriteFile(password, []byte("right-password\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := send(t, d); err != nil {
			t.Fatalf("a send after the password file changed: %v", err)
		}
		expect(t)
	})

	t.Run("ST-SUP-RP01-LC32-T04 OAUTHBEARER reads the token file again at each authentication", func(t *testing.T) {
		expired := kafkatest.Token("svc-orders", time.Now().Add(-time.Minute))
		token := write(t, "token", expired+"\n")
		d := destination(t, Settings{Brokers: []string{broker.SASL}, Topic: topic, CAFile: broker.CAFile,
			Mechanism: MechanismOAuthBearer, TokenFile: token})
		if err := send(t, d); err == nil || err.Error() != errSASL.Error() {
			t.Fatalf("an expired token gave %v, want %q", err, errSASL)
		} else if strings.Contains(err.Error(), expired) || strings.Contains(err.Error(), strings.Split(expired, ".")[1]) {
			t.Fatalf("the error %q holds the token", err)
		}
		if err := os.WriteFile(token, []byte(kafkatest.Token("svc-orders", time.Now().Add(time.Hour))), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := send(t, d); err != nil {
			t.Fatalf("a send after the token file changed: %v", err)
		}
		expect(t)
	})
}
