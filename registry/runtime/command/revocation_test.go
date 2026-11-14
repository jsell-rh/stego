package command

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jsell-rh/stego/registry/runtime/audit"
	"github.com/jsell-rh/stego/registry/runtime/authn"
	"github.com/jsell-rh/stego/registry/runtime/authn/authntest"
	"github.com/jsell-rh/stego/registry/runtime/authz"
	"github.com/jsell-rh/stego/registry/runtime/idempotency"
	"github.com/jsell-rh/stego/registry/runtime/outbox"
	"github.com/jsell-rh/stego/registry/runtime/probe"
	"github.com/jsell-rh/stego/registry/runtime/query"
)

// deliveredPort is a relationships port of the relay that records the resources of the tuples it wrote, and a
// decision port that allows a resource only when a tuple of it was written: a check sees a create only after the
// relay delivered its tuples, as at SpiceDB.
type deliveredPort struct {
	mu        sync.Mutex
	delivered map[string]bool
	n         int
}

func (d *deliveredPort) write(touches []authz.Tuple) (authz.Token, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.delivered == nil {
		d.delivered = map[string]bool{}
	}
	for _, tuple := range touches {
		d.delivered[tuple.Resource.ID] = true
	}
	d.n++
	return authz.NewToken("t-" + strings.Repeat("9", d.n))
}

func (d *deliveredPort) Touch(_ context.Context, tuples []authz.Tuple, _ []authz.Precondition) (authz.Token, error) {
	return d.write(tuples)
}

func (d *deliveredPort) Delete(context.Context, []authz.Filter, []authz.Precondition) (authz.Token, error) {
	return d.write(nil)
}

func (d *deliveredPort) Write(_ context.Context, touches []authz.Tuple, _ []authz.Filter, _ []authz.Precondition) (authz.Token, error) {
	return d.write(touches)
}

func (d *deliveredPort) allowed(r authz.Resource) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.delivered[r.ID] || r.Type == "service"
}

func (d *deliveredPort) Evaluate(_ context.Context, r authz.EvaluationRequest) (authz.EvaluationResult, error) {
	return authz.EvaluationResult{Allowed: d.allowed(r.Resource)}, nil
}

func (d *deliveredPort) EvaluateBatch(_ context.Context, b authz.BatchRequest) ([]authz.EvaluationResult, error) {
	results := make([]authz.EvaluationResult, len(b.Evaluations))
	for i, r := range b.Evaluations {
		results[i] = authz.EvaluationResult{Allowed: d.allowed(r.Resource)}
	}
	return results, nil
}

func (d *deliveredPort) Search(context.Context, authz.SearchRequest) (authz.SearchResult, error) {
	return authz.SearchResult{}, authz.ErrNoSearch
}

func (d *deliveredPort) Searches() bool { return false }

// revocationEnv is the env of the revocation tests: the authorization runtime with the wait cap and the waiter
// limit of the test over the delivered port, the command runtime over it, and the verified identities of two
// principals in two tenants.
type revocationEnv struct {
	*env
	port     *deliveredPort
	az       *authz.Runtime
	rt       *Runtime
	orders   *Entity[placedOrder]
	place    *Command[testProbe]
	identity map[string]context.Context // "subject@tenant": a context with the verified identity
}

func newRevocationEnv(t *testing.T, waitCap time.Duration, waiters int) *revocationEnv {
	t.Helper()
	e := newEnv(t)
	r := &revocationEnv{env: e, port: &deliveredPort{}, identity: map[string]context.Context{}}
	var err error
	if r.az, err = authz.New(authz.Config{Provider: r.port, DB: e.pool, Schema: "authz", Outbox: e.queue,
		Poll: 10 * time.Millisecond, WaitCap: waitCap, Waiters: waiters}); err != nil {
		t.Fatal(err)
	}
	r.rt = e.runtime(func(c *Config) { c.Authz = r.az })
	_, orders, _ := referenceEntities(e)
	s := orders.spec
	s.Parent = nil
	s.Relationships = []Relationship[placedOrder]{{Relation: "owner", SubjectType: "user"}}
	if r.orders, err = NewEntity(s); err != nil {
		t.Fatal(err)
	}
	r.place = e.command("place", func(s *CommandSpec[testProbe]) {
		s.Permission, s.Resource = "place", &Resource{Type: "service", Service: "orders"}
	}, r.orders)
	key := authntest.NewKey(t, "k1")
	set, err := authn.ParseKeys(authntest.Document(key.JWK()))
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := authn.New(authn.Settings{Issuer: "https://issuer.example", Audience: "orders", ProblemCode: "ORDERS",
		Keys: set, TenantClaim: "tenant"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(verifier.Stop)
	for _, who := range []string{"u-7@t1", "u-8@t1", "u-7@t2"} {
		subject, tenantID, _ := strings.Cut(who, "@")
		claims := authntest.Claims("https://issuer.example", "orders", subject, time.Now())
		claims["tenant"] = tenantID
		ctx, err := verifier.Authenticate(context.Background(), "Bearer "+authntest.Mint(t, authntest.Header("k1"), claims, key))
		if err != nil {
			t.Fatal(err)
		}
		r.identity[who] = ctx
	}
	return r
}

// bound is the verified tenant and principal of who.
func (r *revocationEnv) bound(who string) Bound {
	r.t.Helper()
	b, err := BindIdentity(r.identity[who], "user")
	if err != nil {
		r.t.Fatal(err)
	}
	return b
}

// create runs the place command as who with key, which creates the order id when id is not empty, and returns the
// result.
func (r *revocationEnv) create(who, key, id string, fill func(ctx context.Context, h *Handle[testProbe]) (idempotency.Response, error)) Result {
	r.t.Helper()
	b := r.bound(who)
	req := r.request(b.Tenant.ID(), b.Principal, key, `{}`)
	if fill == nil {
		fill = fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
			_, err := repo(r.t, h, r.orders).Create(ctx, placedOrder{ID: id, CustomerID: "c1", ProductID: "p1"})
			return err
		})
	}
	result, err := r.runOn(r.rt, r.place, req, fill)
	if err != nil {
		r.t.Fatal(err)
	}
	return result
}

// startRelay runs the relay of the delivered port in an outbox worker of the worker role until the test ends.
func (r *revocationEnv) startRelay() {
	r.t.Helper()
	relay, err := authz.NewRelay(authz.RelayConfig{Name: "authz-relay", Relationships: r.port,
		DB: r.openPool(r.dsn(r.roles[2])), Schema: "authz"})
	if err != nil {
		r.t.Fatal(err)
	}
	config := outbox.DefaultWorkerConfig()
	config.PollInterval = 10 * time.Millisecond
	config.RetryMin = 10 * time.Millisecond
	config.RetryMax = 50 * time.Millisecond
	config.AttemptTimeout = 2 * time.Second
	worker, err := outbox.NewWorker(r.wqueue, config)
	if err != nil {
		r.t.Fatal(err)
	}
	if err := worker.Register(relay.Destination()); err != nil {
		r.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = worker.Run(ctx) }()
	r.t.Cleanup(func() { cancel(); <-done })
}

// status reads path through the status route as who, or with no identity when who is empty, with the Prefer header
// prefer when it is not empty.
func (r *revocationEnv) status(who, path, prefer string) (*httptest.ResponseRecorder, map[string]string, time.Duration) {
	r.t.Helper()
	ctx := context.Background()
	if who != "" {
		ctx = r.identity[who]
	}
	request := httptest.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	if prefer != "" {
		request.Header.Set("Prefer", prefer)
	}
	answer := httptest.NewRecorder()
	start := time.Now()
	ServeRevocationStatus(r.rt, "user").ServeHTTP(answer, request)
	took := time.Since(start)
	body := map[string]string{}
	if answer.Code == http.StatusOK {
		if err := json.Unmarshal(answer.Body.Bytes(), &body); err != nil {
			r.t.Fatalf("the status answer is not JSON: %v: %s", err, answer.Body)
		}
	}
	return answer, body, took
}

func TestRevocationAnswers(t *testing.T) {
	t.Run("ST-SUP-RP10-LC35-T01 A committed write that enqueued a tuple answers its revocation, and its replay is the same", func(t *testing.T) {
		r := newRevocationEnv(t, 0, 0)
		first := r.create("u-7@t1", "k-lc36-1", "o1", nil)
		location := first.Response.Header.Get(authz.RevocationLocationHeader)
		if first.Response.Header.Get(authz.RevocationHeader) != "pending" || !strings.HasPrefix(location, authz.RevocationPath) {
			t.Fatalf("the create answered %v, want Authz-Revocation: pending and a location under %s", first.Response.Header, authz.RevocationPath)
		}
		if _, err := uuid.Parse(strings.TrimPrefix(location, authz.RevocationPath)); err != nil {
			t.Fatalf("the location %s does not end in a revocation identity: %v", location, err)
		}
		// The relay delivers, and the same key gives the stored answer: still pending, byte for byte.
		r.startRelay()
		if answer, body, _ := r.status("u-7@t1", location, "wait=5"); answer.Code != http.StatusOK || body["state"] != "complete" {
			t.Fatalf("the status after the delivery is %d %v, want complete", answer.Code, body)
		}
		replay := r.create("u-7@t1", "k-lc36-1", "o1", nil)
		if !replay.Replayed || replay.Response.Header.Get(authz.RevocationHeader) != "pending" ||
			replay.Response.Header.Get(authz.RevocationLocationHeader) != location {
			t.Fatalf("the replay answered %v (replayed %v), want the stored pending answer with %s", replay.Response.Header, replay.Replayed, location)
		}
		// A write that enqueues no tuple sends neither header, also when the fill sets one.
		none := r.create("u-7@t1", "k-lc36-2", "", func(context.Context, *Handle[testProbe]) (idempotency.Response, error) {
			return idempotency.Response{Status: http.StatusOK, Header: http.Header{authz.RevocationHeader: {"pending"},
				authz.RevocationLocationHeader: {authz.RevocationPath + uuid.NewString()}}, Body: []byte("{}")}, nil
		})
		if none.Response.Header.Get(authz.RevocationHeader) != "" || none.Response.Header.Get(authz.RevocationLocationHeader) != "" {
			t.Fatalf("a write with no tuple answered %v, want no revocation header", none.Response.Header)
		}
	})
	t.Run("ST-SUP-RP10-LC35-T02 The status route answers only the writer, pending and then complete with the token", func(t *testing.T) {
		r := newRevocationEnv(t, 0, 0)
		location := r.create("u-7@t1", "k-lc36-3", "o3", nil).Response.Header.Get(authz.RevocationLocationHeader)
		if answer, body, _ := r.status("u-7@t1", location, ""); answer.Code != http.StatusOK || body["state"] != "pending" ||
			body["token"] != "" || answer.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("the status before the delivery is %d %v %v, want 200 pending with no token and no-store", answer.Code, body, answer.Header())
		}
		for _, who := range []string{"u-8@t1", "u-7@t2"} {
			if answer, _, _ := r.status(who, location, ""); answer.Code != http.StatusNotFound {
				t.Fatalf("%s read the status of u-7@t1's revocation: %d, want 404", who, answer.Code)
			}
		}
		for _, path := range []string{authz.RevocationPath + uuid.NewString(), authz.RevocationPath + "not-an-id",
			authz.RevocationPath + strings.ToUpper(strings.TrimPrefix(location, authz.RevocationPath))} {
			if answer, _, _ := r.status("u-7@t1", path, ""); answer.Code != http.StatusNotFound {
				t.Fatalf("the status of %s is %d, want 404", path, answer.Code)
			}
		}
		if answer, _, _ := r.status("", location, ""); answer.Code != http.StatusUnauthorized {
			t.Fatalf("the status with no identity is %d, want 401", answer.Code)
		}
		r.startRelay()
		answer, body, _ := r.status("u-7@t1", location, "wait=5")
		if answer.Code != http.StatusOK || body["state"] != "complete" || !strings.HasPrefix(body["token"], "t-9") {
			t.Fatalf("the status after the delivery is %d %v, want complete with the token of the delivery", answer.Code, body)
		}
		if answer.Header().Get("Preference-Applied") != "wait=5" {
			t.Fatalf("Preference-Applied is %q, want wait=5", answer.Header().Get("Preference-Applied"))
		}
	})
}

func TestRevocationWait(t *testing.T) {
	t.Run("ST-SUP-RP06-LC24-T01 WaitFor waits at most the wait cap and at most the waiter limit of calls wait", func(t *testing.T) {
		r := newRevocationEnv(t, 400*time.Millisecond, 2)
		location := r.create("u-7@t1", "k-lc24-1", "o4", nil).Response.Header.Get(authz.RevocationLocationHeader)
		// The relay does not run: the cap cuts a longer wait, and Preference-Applied gives the cap.
		answer, body, took := r.status("u-7@t1", location, "wait=30")
		if body["state"] != "pending" || took < 400*time.Millisecond || took > 3*time.Second {
			t.Fatalf("a wait of 30 s with a cap of 0.4 s answered %v after %s, want pending after the cap", body, took)
		}
		if answer.Header().Get("Preference-Applied") != "wait=0" {
			t.Fatalf("Preference-Applied is %q, want the cap in whole seconds, wait=0", answer.Header().Get("Preference-Applied"))
		}
		// Three waits at one time with a limit of two: one answers at once with no Preference-Applied.
		type outcome struct {
			took    time.Duration
			applied string
		}
		results := make(chan outcome, 3)
		for range 3 {
			go func() {
				answer, _, took := r.status("u-7@t1", location, "wait=1")
				results <- outcome{took, answer.Header().Get("Preference-Applied")}
			}()
		}
		var quick, waited int
		for range 3 {
			o := <-results
			switch {
			case o.took < 200*time.Millisecond && o.applied == "":
				quick++
			case o.took >= 350*time.Millisecond && o.applied != "":
				waited++
			}
		}
		if quick != 1 || waited != 2 {
			t.Fatalf("three waits with a limit of two gave %d at once and %d after the cap, want 1 and 2", quick, waited)
		}
	})
	t.Run("ST-SUP-RP06-LC24-T02 A waiting call holds no pooled connection", func(t *testing.T) {
		// The cap of 2 s keeps every wait open while the test samples the pool, from 0.2 s to 0.7 s.
		r := newRevocationEnv(t, 2*time.Second, 10)
		location := r.create("u-7@t1", "k-lc24-2", "o5", nil).Response.Header.Get(authz.RevocationLocationHeader)
		var wg sync.WaitGroup
		for range 5 {
			wg.Add(1)
			go func() { defer wg.Done(); r.status("u-7@t1", location, "wait=5") }()
		}
		time.Sleep(200 * time.Millisecond)
		// Between two reads a wait holds no connection. The five waits poll at the same times, so a sample can see
		// their five short reads at once; but a wait that held its connection would keep one in use at every
		// sample. So the pool must have no connection in use at some samples.
		idle, busy := 0, 0
		for range 50 {
			if n := r.pool.Stats().InUse; n == 0 {
				idle++
			} else {
				busy = max(busy, n)
			}
			time.Sleep(10 * time.Millisecond)
		}
		wg.Wait()
		if idle == 0 {
			t.Fatalf("five waits kept a pooled connection in use at each of 50 samples (up to %d), want none between two reads", busy)
		}
	})
}

func TestReadYourWrites(t *testing.T) {
	t.Run("ST-SUP-RP11-LC21-T01 Two creates, then an immediate list with the revocation of the last shows both", func(t *testing.T) {
		r := newRevocationEnv(t, 5*time.Second, 0)
		first := r.create("u-7@t1", "k-ryw-1", "ryw1", nil)
		second := r.create("u-7@t1", "k-ryw-2", "ryw2", nil)
		location := second.Response.Header.Get(authz.RevocationLocationHeader)
		if location == "" || location == first.Response.Header.Get(authz.RevocationLocationHeader) {
			t.Fatalf("the creates answered %v and %v, want a revocation of their own each", first.Response.Header, second.Response.Header)
		}
		queries, err := query.New(query.Config{DB: r.pool, AuditDB: r.auditDB, Audit: r.writer,
			AuditSettings: audit.Settings{Level: audit.LevelChange}, Authz: r.az, Service: "orders"})
		if err != nil {
			t.Fatal(err)
		}
		entity, err := query.NewEntity(query.Spec{Name: "order", Schema: "public", Table: "placed_orders", Key: "id",
			Columns: []string{"id"}, ResourceType: "order", ReadPermission: "view", ListPermission: "view"},
			func(row query.Row) (string, error) { return row.Text("id") })
		if err != nil {
			t.Fatal(err)
		}
		list, err := query.NewListQuery("list_orders", entity, "view")
		if err != nil {
			t.Fatal(err)
		}
		b := r.bound("u-7@t1")
		listed := func(header string) ([]string, string) {
			t.Helper()
			request := httptest.NewRequestWithContext(r.identity["u-7@t1"], http.MethodGet, "/orders", nil)
			if header != "" {
				request.Header.Set(authz.RevocationAfterHeader, header)
			}
			answer := httptest.NewRecorder()
			if err := queries.AwaitWrite(answer, request, b.Tenant, b.Principal); err != nil {
				t.Fatal(err)
			}
			call := probe.NewRecorder(nil).Bind(context.Background(), b.Tenant.ID(), b.Principal.Subject().String(), "list_orders")
			ids, err := query.Run(request.Context(), queries, list, query.Bound[struct{}]{Tenant: b.Tenant, Principal: b.Principal,
				Call: call, Probe: func(*probe.Probe) struct{} { return struct{}{} }, RequestID: NewID()}, query.ListRequest{Limit: 10},
				func(ctx context.Context, request query.ListRequest, h *query.Handle[struct{}]) ([]string, error) {
					repository, err := query.Repo(h, entity)
					if err != nil {
						return nil, err
					}
					page, err := repository.List(ctx, request)
					var out []string
					for _, item := range page.Items {
						out = append(out, item.Value())
					}
					return out, err
				})
			if err != nil {
				t.Fatal(err)
			}
			return ids, answer.Header().Get(authz.RevocationHeader)
		}
		// Before the relay runs, a list does not see the creates: this is the gap that the header closes.
		if ids, _ := listed(""); len(ids) != 0 {
			t.Fatalf("a list before the delivery shows %v, want nothing", ids)
		}
		go func() { time.Sleep(300 * time.Millisecond); r.startRelay() }()
		ids, state := listed(location)
		if state != "complete" || len(ids) != 2 {
			t.Fatalf("a list with %s answered %s and %v, want complete and both creates", authz.RevocationAfterHeader, state, ids)
		}
		// The identity of a revocation that is not the caller's own is unknown, and a value that is no identity is
		// a bad request.
		if _, state := listed(authz.RevocationPath + uuid.NewString()); state != query.RevocationUnknown {
			t.Fatalf("a list with a revocation that is not the caller's answered %q, want unknown", state)
		}
		request := httptest.NewRequestWithContext(r.identity["u-7@t1"], http.MethodGet, "/orders", nil)
		request.Header.Set(authz.RevocationAfterHeader, "not-an-id")
		if err := queries.AwaitWrite(httptest.NewRecorder(), request, b.Tenant, b.Principal); !errors.Is(err, query.ErrBadRequest) {
			t.Fatalf("a list with a bad %s gave %v, want ErrBadRequest", authz.RevocationAfterHeader, err)
		}
	})
}
