package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"

	"github.com/jsell-rh/stego/registry/runtime/audit"
	"github.com/jsell-rh/stego/registry/runtime/audit/auditoutbox"
	"github.com/jsell-rh/stego/registry/runtime/authn/authntest"
	"github.com/jsell-rh/stego/registry/runtime/authz"
	"github.com/jsell-rh/stego/registry/runtime/command"
	"github.com/jsell-rh/stego/registry/runtime/idempotency"
	"github.com/jsell-rh/stego/registry/runtime/lease"
	"github.com/jsell-rh/stego/registry/runtime/modes"
	"github.com/jsell-rh/stego/registry/runtime/outbox"
	"github.com/jsell-rh/stego/registry/runtime/probe"
)

// recordingSink is the audit sink of the test: it keeps each event and each anchor that the outbox delivers.
type recordingSink struct {
	mu      sync.Mutex
	events  []audit.Event
	anchors []audit.Anchor
}

func (s *recordingSink) Append(_ context.Context, events []audit.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, events...)
	return nil
}

func (s *recordingSink) Anchor(_ context.Context, a audit.Anchor) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.anchors = append(s.anchors, a)
	return nil
}

func (s *recordingSink) counts() (events, anchors int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events), len(s.anchors)
}

// deliveries are the effects that the destination notify of the test got, each with its tenant.
type deliveries struct {
	mu      sync.Mutex
	tenants []string
}

func (d *deliveries) handler(_ context.Context, _ *sql.Tx, delivery outbox.Delivery) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.tenants = append(d.tenants, delivery.Tenant.ID())
	return nil
}

func (d *deliveries) list() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.tenants...)
}

// workerParts are the parts of the worker and controller modes of the test service, as the generated outbox,
// authorization and audit packages give them.
func workerParts(sink audit.Sink, notify *deliveries) WorkerParts {
	return WorkerParts{
		NewWorker: func(queue *outbox.Queue, config outbox.WorkerConfig) (*outbox.Worker, error) {
			w, err := outbox.NewWorker(queue, config)
			if err != nil {
				return nil, err
			}
			destination, err := audit.NewDestination(sink)
			if err != nil {
				return nil, err
			}
			adapter, err := auditoutbox.NewSink(destination)
			if err != nil {
				return nil, err
			}
			if err := w.Register(adapter.Destination()); err != nil {
				return nil, err
			}
			return w, w.Register(outbox.Destination{Name: "notify", Idempotent: true, Handler: notify.handler})
		},
		NewRelay: func(ctx context.Context, db *sql.DB, relationships authz.Relationships, w *outbox.Worker,
			meter metric.MeterProvider, logger otellog.LoggerProvider) (*authz.Relay, error) {
			relay, err := authz.NewRelay(authz.RelayConfig{Name: "authz-relay", Relationships: relationships, DB: db,
				Schema: "public", Owners: map[string]authz.Owner{"order": {Table: "orders", Key: "id"}}, Meter: meter, Logger: logger})
			if err != nil {
				return nil, err
			}
			if err := relay.Check(ctx); err != nil {
				return nil, err
			}
			return relay, w.Register(relay.Destination())
		},
		WatchInterval: time.Second,
		AuditJobs: func(of audit.LeaseOf, tenants audit.TenantsOf, db *sql.DB, q audit.Enqueuer) (*audit.Anchorer, *audit.Maintainer, error) {
			anchorer, err := audit.NewAnchorer(audit.AnchorConfig{Period: time.Hour, Lease: of, Tenants: tenants, DB: db, Outbox: q})
			if err != nil {
				return nil, nil, err
			}
			maintainer, err := audit.NewMaintainer(audit.MaintainConfig{Lease: of, DB: db, Outbox: q})
			return anchorer, maintainer, err
		},
		AnchorPeriod: time.Hour,
		TenantSchema: "public",
	}
}

// loginURL is the URL of a login of role on the database of the fixture.
func (f *fixture) loginURL(role string) string {
	u := mustURL(f.t, f.database)
	u.User = url.UserPassword(role, rolePassword)
	return u.String()
}

// eventually waits until ok holds, at most 30 s.
func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("after 30 s: %s", what)
}

func TestWorkers(t *testing.T) {
	t.Run("ST-SUP-RP14-LC04-T01 Start runs the workers of the selected modes, and Shutdown stops them once", func(t *testing.T) {
		ctx := context.Background()
		endpoint, key := startSpiceDB(t)
		f := newFixture(t)
		spice := SpiceDB{Endpoint: endpoint, Key: key, InsecureLoopback: true}
		if err := Migrate(ctx, MigrationSettings{DatabaseURL: f.database, SpiceDB: spice}, f.migration(nil)); err != nil {
			t.Fatal(err)
		}
		// What the tenancy component's role migration gives, and a provisioned tenant: logins of the worker and the
		// relay roles, the worker role's grant on the token column of the protected table, and the tenant t1 in the
		// tenant list that Migrate made.
		for _, statement := range []string{
			"ALTER ROLE " + f.worker + " LOGIN PASSWORD '" + rolePassword + "'",
			"ALTER ROLE " + f.relay + " LOGIN PASSWORD '" + rolePassword + "'",
			"GRANT SELECT, UPDATE (authz_token) ON public.orders TO " + f.worker,
			"INSERT INTO public.stego_tenants (tenant_id) VALUES ('t1')",
			"INSERT INTO public.orders (tenant_id, id, status) VALUES ('t1', 'o1', 'open')",
		} {
			if _, err := f.admin.ExecContext(ctx, statement); err != nil {
				t.Fatalf("%.60s: %v", statement, err)
			}
		}
		relate(t, endpoint, key, "order", "o1", "owner", "user:u-7")
		signing := authntest.NewKey(t, "k1")
		settings := Settings{DatabaseURL: f.runtimeURL(), Issuer: "https://issuer.example", Audience: "todo",
			KeysFile: writeFile(t, "keys.json", authntest.Document(signing.JWK())), SpiceDB: spice,
			CursorKey: []byte(strings.Repeat("k", 32)), ServiceVersion: "1.0.0"}
		sink, notify := &recordingSink{}, &deliveries{}
		orders := orderEntity(t)
		p := parts([]command.Declared{orders}, nil)
		p.Workers = workerParts(sink, notify)
		svc, err := New(ctx, settings, p)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = svc.Close() })
		workerSettings := WorkerSettings{WorkerDatabaseURL: f.loginURL(f.worker), RelayDatabaseURL: f.loginURL(f.relay)}
		fast := outbox.DefaultWorkerConfig()
		fast.PollInterval = 20 * time.Millisecond
		leases := lease.Settings{TTL: 3 * time.Second, Renew: time.Second, Retry: 200 * time.Millisecond}

		// A selection that runs the worker mode with no connection string of the worker role is refused by its name.
		if _, err := svc.Start(ctx, StartConfig{Selection: modes.Selection{modes.Worker}}); err == nil || !strings.Contains(err.Error(), WorkerDatabaseURLSetting) || strings.Contains(err.Error(), rolePassword) {
			t.Fatalf("Start of the worker mode with no worker role gave %v", err)
		}
		// The api mode alone starts no worker and holds no lease.
		api, err := svc.Start(ctx, StartConfig{Selection: modes.Selection{modes.API}})
		if err != nil {
			t.Fatal(err)
		}
		if api.Worker() != nil || api.Relay() != nil || api.Periodic() != nil || api.Holds() {
			t.Fatal("the api mode started a component of another mode")
		}
		if err := api.Shutdown(ctx); err != nil {
			t.Fatal(err)
		}

		// One command with an effect and a tuple write, before any worker runs: its messages wait in the outbox.
		cmd, err := command.NewCommand(command.CommandSpec[struct{}]{Name: "share-order", Writes: []command.Declared{orders},
			Probe:        func(*probe.Probe) struct{} { return struct{}{} },
			Destinations: []command.Destination{{Name: "notify", Idempotent: true, Effects: []string{"order.shared"}}},
			Relations:    []authz.Relation{{ResourceType: "order", Name: "owner"}},
			Permission:   "view", Resource: &command.Resource{Type: "order",
				Token:   "SELECT authz_token FROM public.orders WHERE tenant_id = $1 AND id = $2 FOR SHARE",
				ValidID: func(id string) bool { return id != "" }}})
		if err != nil {
			t.Fatal(err)
		}
		t1 := mustTenant(t, "t1")
		fingerprint, err := idempotency.Fingerprint("POST", "/orders/o1/share", []byte(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := command.Run(ctx, svc.Commands(), cmd, command.Request{Tenant: t1, Principal: mustPrincipal(t, "user:u-7"),
			Key: "k-share", Fingerprint: fingerprint, Body: []byte(`{}`), Resource: "o1"},
			func([]byte) (struct{}, error) { return struct{}{}, nil },
			func(ctx context.Context, _ struct{}, h *command.Handle[struct{}]) (idempotency.Response, error) {
				repo, err := command.Repo(h, orders)
				if err != nil {
					return idempotency.Response{}, err
				}
				o1, err := repo.GetForUpdate(ctx, "o1")
				if err != nil {
					return idempotency.Response{}, err
				}
				if _, err := repo.Update(ctx, o1, order{ID: "o1", Status: "shared"}); err != nil {
					return idempotency.Response{}, err
				}
				resource, err := authz.NewResource(t1, "order", "o1")
				if err != nil {
					return idempotency.Response{}, err
				}
				if _, err := h.Authz().Relate(ctx, o1, authz.Tuple{Resource: resource, Relation: "owner",
					Subject: authz.Subject{Type: "user", ID: "u-8"}}); err != nil {
					return idempotency.Response{}, err
				}
				payload, _ := json.Marshal(map[string]string{"order": "o1"})
				if err := h.Effects().Stage(ctx, "notify", "order.shared", o1, payload); err != nil {
					return idempotency.Response{}, err
				}
				return idempotency.Response{Status: 200, Body: []byte(`{}`)}, nil
			}); err != nil {
			t.Fatalf("the command gave %v", err)
		}
		// All modes: the outbox worker on the worker role's pool, the relay on the relay role's pool, and the audit
		// jobs under the controller lease.
		all, err := svc.Start(ctx, StartConfig{Selection: modes.Selection{modes.All}, Workers: workerSettings,
			Holder: "replica-a", LeaseSettings: leases, WorkerConfig: &fast})
		if err != nil {
			t.Fatal(err)
		}
		shutdownDone := false
		t.Cleanup(func() {
			if !shutdownDone {
				_ = all.Shutdown(ctx)
			}
		})
		var role string
		if err := all.workerDB.QueryRowContext(ctx, "SELECT current_user").Scan(&role); err != nil || role != f.worker {
			t.Fatalf("the outbox worker connects as %q, want the worker role %s: %v", role, f.worker, err)
		}
		if err := all.relayDB.QueryRowContext(ctx, "SELECT current_user").Scan(&role); err != nil || role != f.relay {
			t.Fatalf("the relay connects as %q, want the relay role %s: %v", role, f.relay, err)
		}
		eventually(t, "the replica holds the controller lease", all.Holds)
		// The api mode is ready only once its server accepts: the readiness names it until then.
		if err := all.Ready(ctx); err == nil || !strings.Contains(err.Error(), "api") || strings.Contains(err.Error(), "worker:") {
			t.Fatalf("the readiness before the server accepts gave %v, want only the api mode not ready", err)
		}
		server := httptest.NewUnstartedServer(all.Handler())
		server.Listener = all.Readiness().Listener(server.Listener)
		server.Start()
		t.Cleanup(server.Close)
		eventually(t, "every selected mode is ready", func() bool { return all.Ready(ctx) == nil })
		response, err := server.Client().Get(server.URL + "/readyz")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != 200 || strings.TrimSpace(string(body)) != "ready" {
			t.Fatalf("/readyz gave %d %s", response.StatusCode, body)
		}

		// The effect is delivered, the relay writes the relationship to SpiceDB and stores its token on the row, the
		// audit row reaches the sink, and the anchor job anchors the tenant's chain.
		eventually(t, "the effect is delivered in the tenant t1", func() bool {
			got := notify.list()
			return len(got) == 1 && got[0] == "t1"
		})
		conn, err := DialSpiceDB(spice)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		o1, err := authz.ResourceID(t1, "o1")
		if err != nil {
			t.Fatal(err)
		}
		eventually(t, "the relay writes order:o1#owner@user:u-8 to SpiceDB", func() bool {
			answer, err := v1.NewPermissionsServiceClient(conn).CheckPermission(ctx, &v1.CheckPermissionRequest{
				Consistency: &v1.Consistency{Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true}},
				Resource:    &v1.ObjectReference{ObjectType: "order", ObjectId: o1}, Permission: "view",
				Subject: &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: "u-8"}}})
			return err == nil && answer.GetPermissionship() == v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION
		})
		eventually(t, "the relay stores the token on the row", func() bool {
			var token string
			return f.admin.QueryRowContext(ctx, "SELECT authz_token FROM public.orders WHERE tenant_id = 't1' AND id = 'o1'").Scan(&token) == nil && token != ""
		})
		eventually(t, "the audit row of the command reaches the sink", func() bool { events, _ := sink.counts(); return events >= 1 })
		// The chain of t1 holds the command's row and no stored boundary, so the run of the anchor job at the start
		// of the lease anchors it at once.
		eventually(t, "the anchor job anchors the chain of t1", func() bool { _, anchors := sink.counts(); return anchors >= 1 })
		sink.mu.Lock()
		anchored := sink.anchors[0]
		sink.mu.Unlock()
		if anchored.Tenant != "t1" || anchored.Count < 1 {
			t.Fatalf("the anchor is %+v, want one of t1 with the row count", anchored)
		}
		if stats, ok := all.Periodic().Stats(AnchorJobName); !ok || stats.Runs < 1 {
			t.Fatalf("the periodic component ran the anchor job %d times, want at least once at start", stats.Runs)
		}
		if stats, ok := all.Periodic().Stats(MaintainJobName); !ok || stats.Runs < 1 || stats.LastFailed {
			t.Fatalf("the periodic component ran the maintenance %d times, the last failed %v", stats.Runs, stats.LastFailed)
		}

		// A second replica of the controller mode does not take the lease while the first holds it.
		other, err := svc.Start(ctx, StartConfig{Selection: modes.Selection{modes.Controller}, Holder: "replica-b", LeaseSettings: leases})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = other.Shutdown(ctx) })
		time.Sleep(time.Second)
		if other.Holds() {
			t.Fatal("two replicas hold the controller lease")
		}
		if stats, _ := other.Periodic().Stats(AnchorJobName); stats.Runs != 0 {
			t.Fatalf("the replica with no lease ran the anchor job %d times", stats.Runs)
		}

		// A stopped worker makes the worker mode not ready, and names the component.
		worker := all.Worker()
		worker.StopClaiming()
		eventually(t, "the readiness names the stopped outbox worker", func() bool {
			err := all.Ready(ctx)
			return err != nil && strings.Contains(err.Error(), "worker:") && strings.Contains(err.Error(), OutboxWorkerName)
		})

		// Shutdown stops every component once and releases the lease, which the other replica then takes.
		first := all.Shutdown(ctx)
		shutdownDone = true
		if first != nil {
			t.Fatalf("Shutdown gave %v", first)
		}
		if err := all.Shutdown(ctx); !errors.Is(err, first) && err != first {
			t.Fatalf("a second Shutdown gave %v, want the first result", err)
		}
		if all.Holds() {
			t.Fatal("the replica holds the lease after Shutdown")
		}
		for _, c := range all.components {
			if c.running.Load() {
				t.Fatalf("the component %s runs after Shutdown", c.name)
			}
		}
		if err := all.workerDB.PingContext(ctx); err == nil {
			t.Fatal("the worker role's pool is open after Shutdown")
		}
		if err := all.Ready(ctx); err == nil {
			t.Fatal("the replica is ready after Shutdown")
		}
		eventually(t, "the other replica takes the released lease", other.Holds)
	})

	t.Run("ST-SUP-RP14-LC04-T01 ReadWorkerSettings reads the worker and relay connection strings by their fixed names", func(t *testing.T) {
		t.Setenv(WorkerDatabaseURLSetting, "postgres://worker:marker-w@db/x")
		t.Setenv(RelayDatabaseURLSetting, "postgres://relay:marker-r@db/x")
		s, err := ReadWorkerSettings(true)
		if err != nil || s.WorkerDatabaseURL != "postgres://worker:marker-w@db/x" || s.RelayDatabaseURL != "postgres://relay:marker-r@db/x" {
			t.Fatalf("ReadWorkerSettings gave %+v, %v", s, err)
		}
		file := writeFile(t, "worker.url", []byte("postgres://worker:marker-f@db/x\n"))
		t.Setenv(WorkerDatabaseURLSetting, "")
		t.Setenv(WorkerDatabaseURLFileSetting, file)
		t.Setenv(RelayDatabaseURLSetting, "")
		if s, err := ReadWorkerSettings(false); err != nil || s.WorkerDatabaseURL != "postgres://worker:marker-f@db/x" || s.RelayDatabaseURL != "" {
			t.Fatalf("ReadWorkerSettings from the file gave %+v, %v", s, err)
		}
		if _, err := ReadWorkerSettings(true); err == nil || !strings.Contains(err.Error(), RelayDatabaseURLSetting) {
			t.Fatalf("ReadWorkerSettings with no relay setting gave %v", err)
		}
		t.Setenv(WorkerDatabaseURLFileSetting, "")
		if _, err := ReadWorkerSettings(false); err == nil || !strings.Contains(err.Error(), WorkerDatabaseURLSetting) || strings.Contains(err.Error(), "marker") {
			t.Fatalf("ReadWorkerSettings with no worker setting gave %v", err)
		}
	})
}
