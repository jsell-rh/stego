package authz

// The tests of the generated authorization wiring of ST-SUP-RC29: the migration, the tuple writes, the relay that
// NewRelay builds and registers, its start-up check and its watch. They run in the generated package against the
// PostgreSQL server that STEGO_TEST_POSTGRES_DSN names, a superuser URL, and the SpiceDB server that
// STEGO_TEST_SPICEDB_ENDPOINT names, a local serve-testing container that keeps one datastore for each preshared key
// (ST-STD-LOCAL). Each test makes a database of its own with a runtime role, an outbox worker role and a relay role
// of its own, none a superuser, so row-level security and the grants apply; it applies the outbox runtime's
// migration, the protected tables and the emitted Migration there, and drops the database at the end.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	stegoauthz "github.com/jsell-rh/stego/registry/runtime/authz"
	"github.com/jsell-rh/stego/registry/runtime/outbox"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
)

// described is one authz outbox message read back: its operation, its relation and its subject, and the revocation it
// joins.
type described struct {
	Op, Key, Relation, Subject, Revocation string
}

// describe reads the authz outbox messages as their operation, resource key, relation and subject.
func (f *fixture) describe() []described {
	f.t.Helper()
	var out []described
	for _, e := range f.entries() {
		var body struct {
			Op         string `json:"op"`
			Revocation string `json:"revocation"`
			Tuple      struct {
				Relation    string `json:"relation"`
				SubjectType string `json:"subject_type"`
				SubjectID   string `json:"subject_id"`
			} `json:"tuple"`
		}
		if err := json.Unmarshal([]byte(e.Payload), &body); err != nil {
			f.t.Fatal(err)
		}
		if body.Revocation != e.Group {
			f.t.Errorf("the message of %s names the revocation %s and the group %s", e.ResourceKey, body.Revocation, e.Group)
		}
		out = append(out, described{Op: body.Op, Key: e.ResourceKey, Relation: body.Tuple.Relation,
			Subject: body.Tuple.SubjectType + ":" + body.Tuple.SubjectID, Revocation: body.Revocation})
	}
	return out
}

// ownerSubjects is the subject of the owner relationship of one Document row. An empty owner writes no tuple.
type ownerSubjects struct {
	Owner string
}

func subjects(owner string) *ownerSubjects { return &ownerSubjects{Owner: owner} }

// documentRelations are the relations that the writes of the tests bind: the owner relation of document.
var documentRelations = []stegoauthz.Relation{{ResourceType: "document", Name: "owner"}}

// writeOwner enqueues through h the owner tuple changes of the Document row of key from before to after, as the
// command runtime does for a declared relationship (ST-SUP-RP10-LC05): a changed owner deletes its old tuple and
// then touches its new one. The relay tests use it to put tuples in the authz outbox. It gives the revocation and
// whether it enqueued a tuple.
func writeOwner(ctx context.Context, h *stegoauthz.Handle, tn tenant.Tenant, key string, before, after *ownerSubjects) (stegoauthz.Revocation, bool, error) {
	var revocation stegoauthz.Revocation
	resource, err := DocumentResource(tn, key)
	if err != nil {
		return revocation, false, err
	}
	var was, is string
	if before != nil {
		was = before.Owner
	}
	if after != nil {
		is = after.Owner
	}
	if was == is {
		return revocation, false, nil
	}
	wrote := false
	if was != "" {
		if revocation, err = h.Unrelate(ctx, stegoauthz.Tuple{Resource: resource, Relation: "owner", Subject: stegoauthz.Subject{Type: "user", ID: was}}); err != nil {
			return revocation, wrote, err
		}
		wrote = true
	}
	if is != "" {
		if revocation, err = h.Relate(ctx, stegoauthz.Tuple{Resource: resource, Relation: "owner", Subject: stegoauthz.Subject{Type: "user", ID: is}}); err != nil {
			return revocation, wrote, err
		}
		wrote = true
	}
	return revocation, wrote, nil
}

// writes gives the tuple writes of the recorded calls of the fake port, other than the floor marker, as
// touch name@subject or delete name@subject.
func writes(calls []call) []string {
	var out []string
	for _, c := range calls {
		if c.Method != "Write" {
			continue
		}
		for _, d := range c.Deletes {
			out = append(out, "delete "+d)
		}
		for _, touch := range c.Touches {
			if !strings.HasPrefix(touch, "stego_floor:") {
				out = append(out, "touch "+touch)
			}
		}
	}
	return out
}

func resourceName(t *testing.T, tn tenant.Tenant, key string) string {
	t.Helper()
	resource, err := DocumentResource(tn, key)
	if err != nil {
		t.Fatal(err)
	}
	return resource.String() + "#owner"
}

func TestAuthorization(t *testing.T) {
	t.Run("ST-SUP-RC29-OC06-T01 Every protected table gets an authz_token column", func(t *testing.T) {
		f := newFixture(t)
		for table, want := range map[string]bool{"documents": true, "projects": true, "notes": false} {
			var n int
			if err := f.admin.QueryRowContext(f.ctx, "SELECT count(*) FROM information_schema.columns WHERE table_schema = 'public' AND table_name = $1 AND column_name = 'authz_token'", table).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if (n == 1) != want {
				t.Errorf("the table %s has %d authz_token columns, want protected %t", table, n, want)
			}
		}
	})

	t.Run("ST-SUP-RC29-OC06-T02 The destination handler stores the returned token on the owning row", func(t *testing.T) {
		f := newFixture(t)
		port := &fakeRelationships{}
		worker, _ := f.worker(port, 1)
		rt := f.runtimeOf(fakeDecision{})
		tn := f.tenantOf("tenant-a")
		f.change(rt, tn, "d1", insertDocument, nil, subjects("u1"), false)
		f.run(worker)
		f.settled()
		if token := f.rowToken(tn, "d1"); token != "t9" {
			t.Errorf("the Document row's authz_token is %q, want t9", token)
		}
	})

	t.Run("ST-SUP-RC29-OC06-T03 The migration creates the floor table, owned by no tenant", func(t *testing.T) {
		f := newFixture(t)
		var rows, tenantColumns int
		var security bool
		if err := f.admin.QueryRowContext(f.ctx, "SELECT count(*) FROM public.authz_floor").Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if err := f.admin.QueryRowContext(f.ctx, "SELECT count(*) FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'authz_floor' AND column_name = 'tenant_id'").Scan(&tenantColumns); err != nil {
			t.Fatal(err)
		}
		if err := f.admin.QueryRowContext(f.ctx, "SELECT relrowsecurity FROM pg_catalog.pg_class WHERE oid = 'public.authz_floor'::regclass").Scan(&security); err != nil {
			t.Fatal(err)
		}
		if rows != 1 || tenantColumns != 0 || security {
			t.Errorf("authz_floor has %d rows, %d tenant_id columns and row-level security %t; want one row, none and off", rows, tenantColumns, security)
		}
		if _, err := f.admin.ExecContext(f.ctx, "UPDATE public.authz_floor SET token = 'f1'"); err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"t1", "t2"} {
			tn := f.tenantOf(id)
			tx, err := tenant.Begin(f.ctx, f.runtime, tn, nil)
			if err != nil {
				t.Fatal(err)
			}
			var token string
			if err := tx.QueryRowContext(f.ctx, "SELECT token FROM public.authz_floor").Scan(&token); err != nil || token != "f1" {
				t.Errorf("the runtime role reads %q, %v in tenant %s, want the one row f1", token, err, id)
			}
			_ = tx.Rollback()
		}
		_, err := f.runtime.ExecContext(f.ctx, "UPDATE public.authz_floor SET token = 'forged'")
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Errorf("the runtime role's update of the floor gives %v, want permission denied", err)
		}
		if _, err := f.relayDB.ExecContext(f.ctx, "UPDATE public.authz_floor SET token = 'relay'"); err != nil {
			t.Errorf("the relay role cannot update the floor: %v", err)
		}
	})

	t.Run("ST-SUP-RC29-OC07-T01 The destination handler writes with touch and delete only", func(t *testing.T) {
		f := newFixture(t)
		port := &fakeRelationships{}
		worker, _ := f.worker(port, 1)
		f.run(worker)
		rt := f.runtimeOf(fakeDecision{})
		tn := f.tenantOf("tenant-a")
		f.change(rt, tn, "d1", insertDocument, nil, subjects("u1"), false)
		f.change(rt, tn, "d1", updateDocument, subjects("u1"), subjects("u2"), false)
		f.change(rt, tn, "d1", deleteDocument, subjects("u2"), nil, false)
		f.settled()
		owner := resourceName(t, tn, "d1")
		want := []string{"touch " + owner + "@user:u1", "delete " + owner + "@user:u1", "touch " + owner + "@user:u2", "delete " + owner + "@user:u2"}
		got := writes(port.recorded())
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("the provider got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		for _, c := range port.recorded() {
			if c.Method != "Write" && c.Method != "Touch" {
				t.Errorf("the relay called %s", c.Method)
			}
		}
	})

	t.Run("ST-SUP-RC29-OC07-T02 Tuples of one resource key are written in order", func(t *testing.T) {
		f := newFixture(t)
		port := &fakeRelationships{}
		rt := f.runtimeOf(fakeDecision{})
		tn := f.tenantOf("tenant-a")
		f.change(rt, tn, "d1", insertDocument, nil, subjects("u1"), false)
		f.change(rt, tn, "d1", updateDocument, subjects("u1"), subjects("u2"), false)
		f.change(rt, tn, "d1", updateDocument, subjects("u2"), subjects("u3"), false)
		f.change(rt, tn, "d2", insertDocument, nil, subjects("u9"), false)
		first, _ := f.worker(port, 2)
		second, _ := f.worker(port, 2)
		f.run(first)
		f.run(second)
		f.settled()
		owner := resourceName(t, tn, "d1")
		var got []string
		for _, w := range writes(port.recorded()) {
			if strings.Contains(w, owner) {
				got = append(got, w)
			}
		}
		want := []string{"touch " + owner + "@user:u1", "delete " + owner + "@user:u1", "touch " + owner + "@user:u2",
			"delete " + owner + "@user:u2", "touch " + owner + "@user:u3"}
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("the writes of d1 are\n%s\nwant in enqueue order\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	})

	t.Run("ST-SUP-RC29-OC07-T03 A poisoned entry is parked and alerted, and other keys go on", func(t *testing.T) {
		f := newFixture(t)
		tn := f.tenantOf("tenant-a")
		bad, err := DocumentResource(tn, "bad")
		if err != nil {
			t.Fatal(err)
		}
		port := &fakeRelationships{fail: func(tuple stegoauthz.Tuple) bool { return tuple.Resource == bad }}
		worker, _ := f.worker(port, 1)
		rt := f.runtimeOf(fakeDecision{})
		f.change(rt, tn, "bad", insertDocument, nil, subjects("u1"), false)
		f.change(rt, tn, "d1", insertDocument, nil, subjects("u1"), false)
		f.change(rt, tn, "d2", insertDocument, nil, subjects("u2"), false)
		f.run(worker)
		f.settled()
		states := map[string]string{}
		for _, e := range f.entries() {
			states[e.ResourceKey] = e.State
		}
		if states["document:bad"] != "parked" || states["document:d1"] != "done" || states["document:d2"] != "done" {
			t.Errorf("the states are %v, want bad parked and d1 and d2 done", states)
		}
		if n := f.logs.count("outbox.parked"); n != 1 {
			t.Errorf("%d park alerts, want one", n)
		}
		got := strings.Join(writes(port.recorded()), "\n")
		for _, key := range []string{"d1", "d2"} {
			if !strings.Contains(got, resourceName(t, tn, key)) {
				t.Errorf("the tuple of %s is not written:\n%s", key, got)
			}
		}
	})

	t.Run("ST-SUP-RC29-OC07-T04 Running the handler twice for one entry leaves one tuple set", func(t *testing.T) {
		f := newFixture(t)
		spice := spiceDB(t, Schema)
		worker, relay := f.worker(spice.provider, 1)
		_ = worker
		rt := f.runtimeOf(spice.provider)
		tn := f.tenantOf("tenant-a")
		f.change(rt, tn, "d1", insertDocument, nil, subjects("alice"), false)
		claimed, err := f.wqueue.Claim(f.ctx, 1, time.Minute)
		if err != nil || len(claimed) != 1 {
			t.Fatalf("claim the entry: %d entries, %v", len(claimed), err)
		}
		for run := 1; run <= 2; run++ {
			tx, err := tenant.Begin(f.ctx, f.workerDB, claimed[0].Tenant, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := relay.Destination().Handler(f.ctx, tx, claimed[0]); err != nil {
				t.Errorf("run %d of the destination handler: %v", run, err)
			}
			// The first run ends as a crash before the fact is recorded: its transaction rolls back.
			if run == 1 {
				_ = tx.Rollback()
				continue
			}
			if err := tenant.Commit(f.ctx, tx, claimed[0].Tenant); err != nil {
				t.Fatal(err)
			}
		}
		if n := spice.count(t, "document"); n != 1 {
			t.Errorf("the provider holds %d document tuples, want one owner tuple", n)
		}
	})

	t.Run("ST-SUP-RC29-OC07-T05 The emitted schema declares the floor marker, so the relay starts only on a provider whose schema has it", func(t *testing.T) {
		f := newFixture(t)
		declared := strings.TrimSuffix(Schema, "\ndefinition stego_floor {\n\trelation marker: stego_floor\n}\n")
		if declared == Schema || strings.Contains(declared, "stego_floor") {
			t.Fatalf("Schema does not end with the floor marker definition once:\n%s", Schema)
		}
		without := spiceDB(t, declared)
		worker, err := outbox.NewWorker(f.wqueue, f.workerConfig(1))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := NewRelay(f.ctx, f.relayDB, without.provider, worker, nil, nil); err == nil || !strings.Contains(err.Error(), "stego_floor") {
			t.Errorf("NewRelay on a schema without the marker gives %v, want a refusal that names stego_floor", err)
		}
		if n := len(worker.Registrations()); n != 0 {
			t.Errorf("the refused relay registered %d destinations", n)
		}
		with := spiceDB(t, Schema)
		if _, err := NewRelay(f.ctx, f.relayDB, with.provider, worker, nil, nil); err != nil {
			t.Fatalf("NewRelay on the emitted schema: %v", err)
		}
		registrations := worker.Registrations()
		if len(registrations) != 1 || registrations[0].Name != "authz" || !registrations[0].Idempotent || !registrations[0].MustLand {
			t.Errorf("the worker holds %+v, want the destination authz, idempotent and must land", registrations)
		}
	})

	t.Run("ST-SUP-RC29-OC07-T06 The emitted watch raises one alert when a revocation is late", func(t *testing.T) {
		f := newFixture(t)
		port := &fakeRelationships{}
		worker, relay := f.worker(port, 1)
		rt := f.runtimeOf(fakeDecision{})
		tn := f.tenantOf("tenant-a")
		f.change(rt, tn, "d1", insertDocument, nil, subjects("u1"), false)
		ctx, cancel := context.WithCancel(f.ctx)
		done := make(chan error, 1)
		go func() { done <- Watch(ctx, relay, f.oldestAge) }()
		f.eventually(AlertBound+3*time.Second, "no late-revocation alert", func() bool { return f.logs.count("authz.revocation.late") >= 1 })
		time.Sleep(5 * WatchInterval)
		if n := f.logs.count("authz.revocation.late"); n != 1 {
			t.Errorf("%d late-revocation alerts while the revocation stays late, want one", n)
		}
		f.run(worker)
		f.settled()
		time.Sleep(5 * WatchInterval)
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Errorf("Watch ends with %v", err)
		}
		if n := f.logs.count("authz.revocation.late"); n != 1 {
			t.Errorf("%d late-revocation alerts after the delivery, want one", n)
		}
	})

	t.Run("ST-SUP-RC29-OC05-T04 A grant lands through the relay and raises the floor, its revocation completes, and a check at the floor denies", func(t *testing.T) {
		f := newFixture(t)
		spice := spiceDB(t, Schema)
		worker, _ := f.worker(spice.provider, 1)
		f.run(worker)
		rt := f.runtimeOf(spice.provider)
		tn := f.tenantOf("tenant-a")
		resource, err := DocumentResource(tn, "d1")
		if err != nil {
			t.Fatal(err)
		}
		check := func(subject string, consistency stegoauthz.Consistency) bool {
			t.Helper()
			result, err := rt.Port().Evaluate(f.ctx, stegoauthz.EvaluationRequest{Subject: stegoauthz.Subject{Type: "user", ID: subject},
				Action: stegoauthz.Action{Name: "edit"}, Resource: resource, Consistency: consistency})
			if err != nil {
				t.Fatal(err)
			}
			return result.Allowed
		}
		complete := func(revocation stegoauthz.Revocation) stegoauthz.RevocationStatus {
			t.Helper()
			var status stegoauthz.RevocationStatus
			f.eventually(20*time.Second, "the revocation is not complete", func() bool {
				var err error
				if status, err = rt.Status(f.ctx, tn, f.writer(), revocation.ID()); err != nil {
					t.Fatal(err)
				}
				return status.State == stegoauthz.RevocationComplete
			})
			return status
		}
		// The grant: alice owns d1.
		before := f.floor()
		grant, _ := f.change(rt, tn, "d1", insertDocument, nil, subjects("alice"), false)
		granted := complete(grant)
		grantToken := f.rowToken(tn, "d1")
		if grantToken == "" || f.floor() == before || f.floor() != grantToken || granted.Token.Text() != grantToken {
			t.Fatalf("after the grant: floor %q (before %q), row token %q, status token %q; want the floor raised to the row's token", f.floor(), before, grantToken, granted.Token.Text())
		}
		token, err := stegoauthz.ParseToken(grantToken)
		if err != nil {
			t.Fatal(err)
		}
		if !check("alice", stegoauthz.AtLeast(token)) {
			t.Fatal("alice may not edit d1 after the grant")
		}
		// The revocation: the owner moves from alice to bob.
		revocation, _ := f.change(rt, tn, "d1", updateDocument, subjects("alice"), subjects("bob"), false)
		revoked := complete(revocation)
		if f.floor() == grantToken || revoked.Token.Text() != f.floor() {
			t.Errorf("after the revocation: floor %q, status token %q, grant token %q; want the floor raised past the grant", f.floor(), revoked.Token.Text(), grantToken)
		}
		// A check that asks for the grant's token still runs at the floor, so it sees the revocation.
		if check("alice", stegoauthz.AtLeast(token)) {
			t.Error("alice may still edit d1 at the floor after the revocation")
		}
		if !check("bob", stegoauthz.AtLeast(token)) {
			t.Error("bob may not edit d1 after the revocation")
		}
	})
}

const rolePassword = "stego-authz-test"

// fixture is one database with the outbox queue, the protected tables and the emitted authorization tables.
type fixture struct {
	t        *testing.T
	ctx      context.Context
	admin    *sql.DB
	runtime  *sql.DB
	workerDB *sql.DB
	relayDB  *sql.DB
	roles    [3]string // runtime, worker, relay
	queue    *outbox.Queue
	wqueue   *outbox.Queue
	logs     *logRecorder
	reader   *sdkmetric.ManualReader
	meter    *sdkmetric.MeterProvider
	logger   *sdklog.LoggerProvider
}

// needPostgres gives the superuser URL of the test server, and skips or fails the test without one.
func needPostgres(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("STEGO_TEST_POSTGRES_DSN")
	if dsn == "" {
		if os.Getenv("STEGO_REQUIRE_POSTGRES") == "1" {
			t.Fatal("the PostgreSQL tests need STEGO_TEST_POSTGRES_DSN")
		}
		t.Skip("set STEGO_TEST_POSTGRES_DSN to run the PostgreSQL tests")
	}
	return dsn
}

func randomTag(t *testing.T) string {
	t.Helper()
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(suffix)
}

// newFixture makes the database of one test and applies, as the superuser, the outbox runtime's migration, the
// protected tables documents and projects and the unprotected table notes, each with the tenant policy of the
// tenancy component and without authz_token, and then the emitted Migration.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	dsn := needPostgres(t)
	ctx := context.Background()
	root, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	tag := randomTag(t)
	roles := [3]string{"authz_rt_" + tag, "authz_worker_" + tag, "authz_relay_" + tag}
	database := "authz_" + tag
	if _, err := root.ExecContext(ctx, "CREATE DATABASE "+database); err != nil {
		t.Fatalf("create the database: %v", err)
	}
	for _, role := range roles {
		if _, err := root.ExecContext(ctx, "CREATE ROLE "+role+" LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD '"+rolePassword+"'"); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = root.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+database+" WITH (FORCE)")
		for _, role := range roles {
			_, _ = root.ExecContext(context.Background(), "DROP ROLE IF EXISTS "+role)
		}
	})
	open := func(role, password string) *sql.DB {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal(err)
		}
		u.Path = "/" + database
		if role != "" {
			u.User = url.UserPassword(role, password)
		}
		db, err := sql.Open("pgx", u.String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	f := &fixture{t: t, ctx: ctx, admin: open("", ""), roles: roles, logs: &logRecorder{}, reader: sdkmetric.NewManualReader()}
	// The outbox relay role of the test service, one role of the server, which the outbox migration makes.
	queueDDL, err := outbox.Migration("authz_check"+outbox.RelaySuffix, roles[0], roles[1])
	if err != nil {
		t.Fatal(err)
	}
	migration, err := Migration(roles[0], roles[2], "authz_check"+outbox.RelaySuffix, roles[1])
	if err != nil {
		t.Fatal(err)
	}
	statements := []string{queueDDL}
	for _, table := range []string{"documents", "projects", "notes"} {
		statements = append(statements,
			"CREATE TABLE public."+table+" (tenant_id text NOT NULL, id text NOT NULL, owner_id text NOT NULL DEFAULT '', PRIMARY KEY (tenant_id, id))",
			"ALTER TABLE public."+table+" ENABLE ROW LEVEL SECURITY",
			"ALTER TABLE public."+table+" FORCE ROW LEVEL SECURITY",
			"CREATE POLICY stego_tenant ON public."+table+" USING (tenant_id = pg_catalog.current_setting('app.tenant', true)) WITH CHECK (tenant_id = pg_catalog.current_setting('app.tenant', true))",
			"GRANT SELECT, INSERT, UPDATE, DELETE ON public."+table+" TO "+roles[0])
	}
	statements = append(statements, migration)
	for _, statement := range statements {
		if _, err := f.admin.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%.120s: %v", statement, err)
		}
	}
	f.runtime, f.workerDB, f.relayDB = open(roles[0], rolePassword), open(roles[1], rolePassword), open(roles[2], rolePassword)
	if f.queue, err = outbox.New(f.runtime, outbox.Options{}); err != nil {
		t.Fatal(err)
	}
	if f.wqueue, err = outbox.New(f.workerDB, outbox.Options{}); err != nil {
		t.Fatal(err)
	}
	f.meter = sdkmetric.NewMeterProvider(sdkmetric.WithReader(f.reader))
	f.logger = sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(f.logs)))
	return f
}

// tenantOf gives the tenant of the ID, read back from a tenant column as the runtime builds one.
func (f *fixture) tenantOf(id string) tenant.Tenant {
	f.t.Helper()
	rows, err := f.admin.QueryContext(f.ctx, "SELECT $1::text", id)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		f.t.Fatal("no row")
	}
	tn, err := tenant.ScanRows(rows)
	if err != nil {
		f.t.Fatal(err)
	}
	return tn
}

// workerConfig is the configuration of the outbox worker of the tests: short polls and retries, and the meter and
// the logger of the fixture.
func (f *fixture) workerConfig(concurrency int) outbox.WorkerConfig {
	config := outbox.DefaultWorkerConfig()
	config.Concurrency = concurrency
	config.PollInterval = 10 * time.Millisecond
	config.RetryMin = 10 * time.Millisecond
	config.RetryMax = 50 * time.Millisecond
	config.AttemptTimeout = 2 * time.Second
	config.LeaseDuration = 30 * time.Second
	config.Meter = f.meter
	config.Logger = f.logger
	return config
}

// worker builds the outbox worker of the worker role and the emitted relay on it, through NewRelay.
func (f *fixture) worker(relationships stegoauthz.Relationships, concurrency int) (*outbox.Worker, *stegoauthz.Relay) {
	f.t.Helper()
	worker, err := outbox.NewWorker(f.wqueue, f.workerConfig(concurrency))
	if err != nil {
		f.t.Fatal(err)
	}
	relay, err := NewRelay(f.ctx, f.relayDB, relationships, worker, f.meter, f.logger)
	if err != nil {
		f.t.Fatal(err)
	}
	return worker, relay
}

// run runs worker until the test ends.
func (f *fixture) run(worker *outbox.Worker) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = worker.Run(ctx) }()
	f.t.Cleanup(func() { cancel(); <-done })
}

// runtimeOf builds the emitted authorization runtime on the runtime role with the decision provider.
func (f *fixture) runtimeOf(provider stegoauthz.Decision) *stegoauthz.Runtime {
	f.t.Helper()
	rt, err := NewRuntime(f.runtime, provider, f.queue, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	return rt
}

// writer is the principal of the writes of the tests.
func (f *fixture) writer() stegoauthz.Principal {
	f.t.Helper()
	p, err := stegoauthz.System("documents-api")
	if err != nil {
		f.t.Fatal(err)
	}
	return p
}

// change runs one transaction of tenant tn as the runtime role: it runs rows on the documents table and then the
// owner tuple writes of writeOwner from before to after, and commits, or rolls back when rollback is true.
func (f *fixture) change(rt *stegoauthz.Runtime, tn tenant.Tenant, key string, rows string, before, after *ownerSubjects, rollback bool) (stegoauthz.Revocation, bool) {
	f.t.Helper()
	tx, err := tenant.Begin(f.ctx, f.runtime, tn, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if rows != "" {
		owner := ""
		if after != nil {
			owner = after.Owner
		}
		if _, err := tx.ExecContext(f.ctx, rows, tn.ID(), key, owner); err != nil {
			f.t.Fatalf("%s: %v", rows, err)
		}
	}
	h, err := rt.Bind(tx, tn, f.writer(), documentRelations)
	if err != nil {
		f.t.Fatal(err)
	}
	revocation, wrote, err := writeOwner(f.ctx, h, tn, key, before, after)
	if err != nil {
		f.t.Fatal(err)
	}
	if rollback {
		return revocation, wrote
	}
	if err := tenant.Commit(f.ctx, tx, tn); err != nil {
		f.t.Fatal(err)
	}
	return revocation, wrote
}

const (
	insertDocument = "INSERT INTO public.documents (tenant_id, id, owner_id) VALUES ($1, $2, $3)"
	updateDocument = "UPDATE public.documents SET owner_id = $3 WHERE tenant_id = $1 AND id = $2"
	deleteDocument = "DELETE FROM public.documents WHERE tenant_id = $1 AND id = $2 AND $3 = $3"
)

// entry is one authz outbox message as the superuser reads it.
type entry struct {
	Kind, ResourceKey, State, Group string
	Payload                         string
}

// entries reads the authz outbox messages in enqueue order.
func (f *fixture) entries() []entry {
	f.t.Helper()
	rows, err := f.admin.QueryContext(f.ctx, "SELECT kind, resource_key, state, coalesce(group_id, ''), convert_from(payload, 'UTF8') FROM stego_outbox.messages WHERE destination = 'authz' ORDER BY sequence")
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []entry
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.Kind, &e.ResourceKey, &e.State, &e.Group, &e.Payload); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatal(err)
	}
	return out
}

// floor reads the revocation floor as the superuser.
func (f *fixture) floor() string {
	f.t.Helper()
	var token string
	if err := f.admin.QueryRowContext(f.ctx, "SELECT token FROM public.authz_floor").Scan(&token); err != nil {
		f.t.Fatal(err)
	}
	return token
}

// rowToken reads authz_token of a documents row as the superuser.
func (f *fixture) rowToken(tn tenant.Tenant, key string) string {
	f.t.Helper()
	var token string
	if err := f.admin.QueryRowContext(f.ctx, "SELECT authz_token FROM public.documents WHERE tenant_id = $1 AND id = $2", tn.ID(), key).Scan(&token); err != nil {
		f.t.Fatal(err)
	}
	return token
}

// eventually waits up to limit for done, and fails the test with what otherwise.
func (f *fixture) eventually(limit time.Duration, what string, done func() bool) {
	f.t.Helper()
	deadline := time.Now().Add(limit)
	for !done() {
		if time.Now().After(deadline) {
			f.t.Fatalf("after %s: %s", limit, what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// settled waits until no authz message is pending.
func (f *fixture) settled() {
	f.t.Helper()
	f.eventually(20*time.Second, "authz messages are still not delivered", func() bool {
		for _, e := range f.entries() {
			if e.State != "done" && e.State != "parked" {
				return false
			}
		}
		return true
	})
}

// call is one call of the fake relationships port.
type call struct {
	Method  string
	Touches []string
	Deletes []string
}

// fakeRelationships is a relationships port in process that records each call and gives the token t9. fail, when set, refuses the write of a tuple whose resource it names, as the provider refuses a malformed one.
type fakeRelationships struct {
	mu    sync.Mutex
	calls []call
	fail  func(stegoauthz.Tuple) bool
}

func (p *fakeRelationships) token() (stegoauthz.Token, error) { return stegoauthz.NewToken("t9") }

func (p *fakeRelationships) Touch(_ context.Context, tuples []stegoauthz.Tuple, _ []stegoauthz.Precondition) (stegoauthz.Token, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c := call{Method: "Touch"}
	for _, tuple := range tuples {
		c.Touches = append(c.Touches, tuple.Name()+"@"+tuple.Subject.String())
	}
	p.calls = append(p.calls, c)
	return p.token()
}

func (p *fakeRelationships) Delete(_ context.Context, filters []stegoauthz.Filter, _ []stegoauthz.Precondition) (stegoauthz.Token, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c := call{Method: "Delete"}
	for _, filter := range filters {
		c.Deletes = append(c.Deletes, filter.ResourceType+":"+filter.ResourceID+"#"+filter.Relation+"@"+filter.SubjectType+":"+filter.SubjectID)
	}
	p.calls = append(p.calls, c)
	return p.token()
}

func (p *fakeRelationships) Write(_ context.Context, touches []stegoauthz.Tuple, deletes []stegoauthz.Filter, _ []stegoauthz.Precondition) (stegoauthz.Token, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, tuple := range touches {
		if p.fail != nil && p.fail(tuple) {
			return stegoauthz.Token{}, stegoauthz.Refused(errors.New("INVALID_ARGUMENT: the tuple is malformed"))
		}
	}
	c := call{Method: "Write"}
	for _, tuple := range touches {
		c.Touches = append(c.Touches, tuple.Name()+"@"+tuple.Subject.String())
	}
	for _, filter := range deletes {
		c.Deletes = append(c.Deletes, filter.ResourceType+":"+filter.ResourceID+"#"+filter.Relation+"@"+filter.SubjectType+":"+filter.SubjectID)
	}
	p.calls = append(p.calls, c)
	return p.token()
}

// recorded gives a copy of the calls so far.
func (p *fakeRelationships) recorded() []call {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]call(nil), p.calls...)
}

// logRecorder records the log records that the relay and the outbox worker emit.
type logRecorder struct {
	mu     sync.Mutex
	events []string
}

func (l *logRecorder) Export(_ context.Context, records []sdklog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range records {
		l.events = append(l.events, r.EventName())
	}
	return nil
}

func (*logRecorder) Shutdown(context.Context) error   { return nil }
func (*logRecorder) ForceFlush(context.Context) error { return nil }

// count gives the number of events of the name.
func (l *logRecorder) count(name string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, event := range l.events {
		if event == name {
			n++
		}
	}
	return n
}

// oldestAge reads the gauge outbox.oldest.age{destination=authz} of the worker's meter.
func (f *fixture) oldestAge(ctx context.Context) (time.Duration, error) {
	var data metricdata.ResourceMetrics
	if err := f.reader.Collect(ctx, &data); err != nil {
		return 0, err
	}
	for _, scope := range data.ScopeMetrics {
		for _, m := range scope.Metrics {
			gauge, ok := m.Data.(metricdata.Gauge[float64])
			if !ok || m.Name != "outbox.oldest.age" {
				continue
			}
			for _, point := range gauge.DataPoints {
				if destination, _ := point.Attributes.Value("destination"); destination.AsString() == "authz" {
					return time.Duration(point.Value * float64(time.Second)), nil
				}
			}
		}
	}
	return 0, nil
}

// bearer sends the preshared key of the local SpiceDB server on each call.
type bearer string

func (b bearer) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(b)}, nil
}

func (bearer) RequireTransportSecurity() bool { return false }

// spiceEnv is one datastore of the local SpiceDB server: the provider and the client that reads relationships.
type spiceEnv struct {
	provider *stegoauthz.SpiceDB
	perms    v1.PermissionsServiceClient
	conn     *grpc.ClientConn
}

// spiceDB connects the test to a datastore of its own on the local SpiceDB server and writes schema to it, as the
// deployment writes the emitted Schema. It skips or fails the test without the server.
func spiceDB(t *testing.T, schema string) *spiceEnv {
	t.Helper()
	endpoint := os.Getenv("STEGO_TEST_SPICEDB_ENDPOINT")
	if endpoint == "" {
		if os.Getenv("STEGO_REQUIRE_POSTGRES") == "1" {
			t.Fatal("the SpiceDB tests need STEGO_TEST_SPICEDB_ENDPOINT")
		}
		t.Skip("set STEGO_TEST_SPICEDB_ENDPOINT to run the SpiceDB tests")
	}
	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(bearer(randomTag(t)+randomTag(t))))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	deadline := time.Now().Add(60 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if schema == "" {
			// An empty datastore: the server is ready when it answers that it holds no schema.
			_, err = v1.NewSchemaServiceClient(conn).ReadSchema(ctx, &v1.ReadSchemaRequest{})
			if status.Code(err) == codes.NotFound {
				err = nil
			}
		} else {
			_, err = v1.NewSchemaServiceClient(conn).WriteSchema(ctx, &v1.WriteSchemaRequest{Schema: schema})
		}
		cancel()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("write the schema to the SpiceDB server: %v", err)
		}
		time.Sleep(250 * time.Millisecond)
	}
	provider, err := stegoauthz.NewSpiceDB(conn)
	if err != nil {
		t.Fatal(err)
	}
	return &spiceEnv{provider: provider, perms: v1.NewPermissionsServiceClient(conn), conn: conn}
}

// count reads the relationships of the resource type at the newest state of the provider.
func (e *spiceEnv) count(t *testing.T, resourceType string) int {
	t.Helper()
	stream, err := e.perms.ReadRelationships(context.Background(), &v1.ReadRelationshipsRequest{
		Consistency:        &v1.Consistency{Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true}},
		RelationshipFilter: &v1.RelationshipFilter{ResourceType: resourceType},
	})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for {
		_, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return n
		}
		if err != nil {
			t.Fatal(err)
		}
		n++
	}
}

// fakeDecision is a decision port in process that denies every request; the relay tests decide nothing.
type fakeDecision struct{}

func (fakeDecision) Evaluate(context.Context, stegoauthz.EvaluationRequest) (stegoauthz.EvaluationResult, error) {
	return stegoauthz.EvaluationResult{}, nil
}

func (fakeDecision) EvaluateBatch(_ context.Context, request stegoauthz.BatchRequest) ([]stegoauthz.EvaluationResult, error) {
	return nil, nil
}

func (fakeDecision) Search(context.Context, stegoauthz.SearchRequest) (stegoauthz.SearchResult, error) {
	return stegoauthz.SearchResult{}, stegoauthz.ErrNoSearch
}

func (fakeDecision) Searches() bool { return false }

// schemaText reads the schema that the provider holds.
func (e *spiceEnv) schemaText(t *testing.T) string {
	t.Helper()
	read, err := v1.NewSchemaServiceClient(e.conn).ReadSchema(context.Background(), &v1.ReadSchemaRequest{})
	if status.Code(err) == codes.NotFound {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return read.GetSchemaText()
}

func TestSchemaWrite(t *testing.T) {
	t.Run("ST-SUP-RC29-OC23-T01 The migration job writes the schema, and refuses one that removes a definition or relation with relationships", func(t *testing.T) {
		needPostgres(t)
		ctx := context.Background()
		archive := "\ndefinition archive {\n\trelation owner: user\n}\n"
		editor := strings.Replace(Schema, "definition document {\n", "definition document {\n\trelation editor: user\n", 1)
		if editor == Schema {
			t.Fatal("Schema has no definition document")
		}
		marker := stegoauthz.Tuple{Resource: stegoauthz.Resource{Type: "stego_floor", ID: "floor"}, Relation: "marker",
			Subject: stegoauthz.Subject{Type: "stego_floor", ID: "floor"}}
		for _, c := range []struct {
			name, held string
			tuple      *stegoauthz.Tuple
			refused    bool
		}{
			{"an empty datastore", "", nil, false},
			{"a removed definition with no relationship", Schema + archive, nil, false},
			{"a removed definition with a relationship", Schema + archive, &stegoauthz.Tuple{Resource: stegoauthz.Resource{Type: "archive", ID: "a1"},
				Relation: "owner", Subject: stegoauthz.Subject{Type: "user", ID: "u1"}}, true},
			{"a removed relation with a relationship", editor, &stegoauthz.Tuple{Resource: stegoauthz.Resource{Type: "document", ID: "d1"},
				Relation: "editor", Subject: stegoauthz.Subject{Type: "user", ID: "u1"}}, true},
		} {
			t.Run(c.name, func(t *testing.T) {
				spice := spiceDB(t, c.held)
				if c.tuple != nil {
					if _, err := spice.provider.Touch(ctx, []stegoauthz.Tuple{*c.tuple}, nil); err != nil {
						t.Fatal(err)
					}
				}
				before := spice.schemaText(t)
				err := WriteSchema(ctx, spice.conn)
				after := spice.schemaText(t)
				if c.refused {
					if err == nil || !strings.Contains(err.Error(), "refuses the schema") {
						t.Fatalf("WriteSchema gives %v, want the refusal", err)
					}
					if after != before {
						t.Errorf("the refused write changed the schema:\n%s", after)
					}
					return
				}
				if err != nil {
					t.Fatalf("WriteSchema: %v", err)
				}
				if strings.Contains(after, "archive") || !strings.Contains(after, "definition stego_floor") {
					t.Errorf("the provider holds\n%s\nwant the emitted Schema", after)
				}
				if _, err := spice.provider.Touch(ctx, []stegoauthz.Tuple{marker}, nil); err != nil {
					t.Errorf("the floor marker cannot be written after WriteSchema: %v", err)
				}
			})
		}
	})
}
