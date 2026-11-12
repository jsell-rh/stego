package query

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/jsell-rh/stego/registry/runtime/authz"
	"github.com/jsell-rh/stego/registry/runtime/internal/pgtest"
	"github.com/jsell-rh/stego/registry/runtime/outbox"
)

// unitDriver is a database/sql driver of the unit tests. It answers only the two reads that a handle makes outside
// the entity tables: the access mode of the transaction, which is READ ONLY, and the revocation floor, which is the
// token that its data source name names.
type unitDriver struct{}

var (
	registerUnit sync.Once
	unitFloors   sync.Map // data source name -> floor token text
)

func (unitDriver) Open(name string) (driver.Conn, error) {
	floor, ok := unitFloors.Load(name)
	if !ok {
		return nil, errors.New("no unit source")
	}
	return &unitConn{floor: floor.(string)}, nil
}

type unitConn struct {
	floor  string
	tenant string
}

func (c *unitConn) Prepare(query string) (driver.Stmt, error) {
	switch {
	case strings.Contains(query, "authz_floor"):
		return unitStmt{value: func([]driver.Value) string { return c.floor }}, nil
	case strings.Contains(query, "transaction_isolation"):
		return unitStmt{value: func([]driver.Value) string { return "read committed" }}, nil
	case strings.Contains(query, "transaction_read_only"):
		return unitStmt{value: func([]driver.Value) string { return "on" }}, nil
	case strings.Contains(query, "set_config('app.tenant'"):
		// tenant.Begin sets the tenant; the connection keeps it for the check of tenant.Commit.
		return unitStmt{value: func(args []driver.Value) string {
			c.tenant, _ = args[0].(string)
			return c.tenant
		}}, nil
	case strings.Contains(query, "current_setting('app.tenant'"):
		return unitStmt{value: func([]driver.Value) string { return c.tenant }}, nil
	case query == statementTimeoutSQL:
		// Run sets the statement time limit of the query's transaction (ST-SUP-RP11-LC20).
		return unitStmt{value: func([]driver.Value) string { return "" }, exec: true}, nil
	}
	return nil, errors.New("the unit driver answers only the floor, the access mode, the tenant setting and the statement time limit")
}
func (c *unitConn) Close() error              { return nil }
func (c *unitConn) Begin() (driver.Tx, error) { return unitTx{}, nil }
func (c *unitConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return unitTx{}, nil
}

type unitStmt struct {
	value func([]driver.Value) string
	exec  bool
}

func (unitStmt) Close() error  { return nil }
func (unitStmt) NumInput() int { return -1 }
func (s unitStmt) Exec(args []driver.Value) (driver.Result, error) {
	// Only the setting of the statement time limit runs as an Exec.
	if s.exec {
		return driver.RowsAffected(0), nil
	}
	return nil, errors.New("no exec")
}
func (s unitStmt) Query(args []driver.Value) (driver.Rows, error) {
	return &unitRows{value: s.value(args)}, nil
}

type unitRows struct {
	value string
	done  bool
}

func (*unitRows) Columns() []string { return []string{"value"} }
func (*unitRows) Close() error      { return nil }
func (r *unitRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = r.value
	return nil
}

type unitTx struct{}

func (unitTx) Commit() error   { return nil }
func (unitTx) Rollback() error { return nil }

func unitDB(t *testing.T, floor string) *sql.DB {
	t.Helper()
	registerUnit.Do(func() { sql.Register("query-unit", unitDriver{}) })
	name := uuid.NewString()
	unitFloors.Store(name, floor)
	db, err := sql.Open("query-unit", name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close(); unitFloors.Delete(name) })
	return db
}

// unitRuntime is a query runtime over the unit driver with the decision port provider, built as New builds it, with
// no audit, for a test that reads no entity with a pii field.
func unitRuntime(t *testing.T, provider authz.Decision) *Runtime {
	t.Helper()
	queue, err := outbox.New(unitDB(t, ""), outbox.Options{})
	if err != nil {
		t.Fatal(err)
	}
	az, err := authz.New(authz.Config{Provider: provider, DB: unitDB(t, ""), Schema: "authz", Outbox: queue})
	if err != nil {
		t.Fatal(err)
	}
	return &Runtime{authz: az}
}

// unitTxOf opens a transaction of the unit driver whose floor reads give floor.
func unitTxOf(t *testing.T, floor string) *sql.Tx {
	t.Helper()
	tx, err := unitDB(t, floor).Begin()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	return tx
}

func mustToken(t *testing.T, text string) authz.Token {
	t.Helper()
	token, err := authz.ParseToken(text)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// consistencyOf is the consistency of a request as text: fresh or at_least:<token>.
func consistencyOf(c authz.Consistency) string {
	if token, ok := c.Token(); ok {
		return "at_least:" + token.Text()
	}
	return c.String()
}

func TestAuthz(t *testing.T) {
	t.Run("ST-SUP-RP11-LC05-T01 A query fill's decision uses the principal and the row token", func(t *testing.T) {
		port := &fakeDecision{decide: allowAll}
		rt := unitRuntime(t, port)
		orders := orderEntity(t, false)
		h, err := bind(context.Background(), rt, unitTxOf(t, ""), mustTenant(t, "t1"), mustPrincipal(t, "user:u-7"),
			mustQuery(t, "order-detail", orders), noProbe{})
		if err != nil {
			t.Fatal(err)
		}
		order, err := newLoaded(h.core, &orders.e, "o-1", mustToken(t, "t5"), Row{columns: []string{"id", "total"}, values: []any{"o-1", int64(0)}}, orders.load)
		if err != nil {
			t.Fatal(err)
		}
		allowed, err := h.Authz().Evaluate(context.Background(), "view", order)
		if err != nil || !allowed {
			t.Fatalf("Evaluate gives %v, %v; want an allow", allowed, err)
		}
		calls := port.recorded()
		if len(calls) != 1 {
			t.Fatalf("%d decision port calls, want 1", len(calls))
		}
		c := calls[0]
		if c.Subject.String() != "user:u-7" || c.Action.Name != "view" || c.Resource.Type+":"+keyOf(c.Resource) != "order:o-1" {
			t.Errorf("the call is %s %s %s, want user:u-7 view order:o-1", c.Subject, c.Action.Name, c.Resource)
		}
		if got := consistencyOf(c.Consistency); got != "at_least:t5" {
			t.Errorf("the consistency is %s, want at_least:t5", got)
		}
		// A value that the handle did not load carries no row token that Evaluate takes.
		if _, err := h.Authz().Evaluate(context.Background(), "view", zeroOf(order)); !errors.Is(err, errForeignValue) {
			t.Errorf("Evaluate of a value that new made gives %v, want errForeignValue", err)
		}
		if len(port.recorded()) != 1 {
			t.Errorf("a refused value reached the decision port")
		}
	})

	t.Run("ST-SUP-RP11-LC05-T02 The query handle has no bypass and no relationship write", func(t *testing.T) {
		typ := reflect.TypeOf(&Authz{})
		if typ.NumMethod() == 0 {
			t.Fatal("the authorization part has no method; the check reads nothing")
		}
		for _, typ := range []reflect.Type{typ, typ.Elem()} {
			for i := 0; i < typ.NumMethod(); i++ {
				name := strings.ToLower(typ.Method(i).Name)
				for _, word := range []string{"bypass", "skip", "admin", "system", "relate", "unrelate"} {
					if strings.Contains(name, word) {
						t.Errorf("the authorization part has the method %s", typ.Method(i).Name)
					}
				}
			}
			if typ.Kind() == reflect.Struct {
				for i := 0; i < typ.NumField(); i++ {
					if typ.Field(i).IsExported() {
						t.Errorf("the authorization part has the exported field %s", typ.Field(i).Name)
					}
				}
			}
		}
	})

	t.Run("ST-SUP-RP11-LC06-T01 A denied query read returns no data", func(t *testing.T) {
		e := newEnv(t)
		e.insert(t, "INSERT INTO app.orders (tenant_id, id, total, authz_token) VALUES ('t1', 'o1', 4200, 't3')")
		port := &fakeDecision{decide: func(r authz.EvaluationRequest) bool { return keyOf(r.Resource) != "o1" }}
		rt := e.runtimeOf(t, port)
		orders := orderEntity(t, false)
		q := mustQuery(t, "order-detail", orders)
		got, err := Run(context.Background(), rt, q, bound(t, "t1", "user:u-7", "order-detail"), "o1",
			func(ctx context.Context, id string, h *Handle[noProbe]) (Loaded[Order], error) {
				repo, err := Repo(h, orders)
				if err != nil {
					return nil, err
				}
				order, err := repo.Get(ctx, id)
				if !errors.Is(err, ErrNotFound) {
					t.Errorf("Get of the denied order gives %v, want ErrNotFound", err)
				}
				return order, nil
			})
		if err != nil {
			t.Fatal(err)
		}
		if got != nil || got.Value().Total != 0 {
			t.Errorf("the fill got %+v of the denied order, want nothing", got.Value())
		}
		calls := port.recorded()
		if len(calls) != 1 || calls[0].Action.Name != "view" || calls[0].Resource.Type+":"+keyOf(calls[0].Resource) != "order:o1" {
			t.Errorf("the decision port calls are %v, want one for view on order:o1", calls)
		}
	})

	t.Run("ST-SUP-RP11-LC06-T01 A denied query read returns no data, decided by SpiceDB", func(t *testing.T) {
		spice := startSpiceDB(t)
		spice.grant(t, "o2", "u-7")
		e := newEnv(t)
		e.insert(t, "INSERT INTO app.orders (tenant_id, id, total) VALUES ('t1', 'o1', 4200), ('t1', 'o2', 99)")
		port := &recordingDecision{Decision: spice.provider}
		rt := e.runtimeOf(t, port)
		orders := orderEntity(t, false)
		q := mustQuery(t, "order-detail", orders)
		_, err := Run(context.Background(), rt, q, bound(t, "t1", "user:u-7", "order-detail"), struct{}{},
			func(ctx context.Context, _ struct{}, h *Handle[noProbe]) (struct{}, error) {
				repo, err := Repo(h, orders)
				if err != nil {
					return struct{}{}, err
				}
				if order, err := repo.Get(ctx, "o1"); !errors.Is(err, ErrNotFound) || order != nil {
					t.Errorf("Get of o1, which SpiceDB denies, gives %v, %v; want ErrNotFound and no value", order, err)
				}
				order, err := repo.Get(ctx, "o2")
				if err != nil || order.Value().Total != 99 {
					t.Errorf("Get of o2, which SpiceDB allows, gives %v, %v", order, err)
				}
				return struct{}{}, nil
			})
		if err != nil {
			t.Fatal(err)
		}
		if n := len(port.recorded()); n != 2 {
			t.Errorf("%d evaluations reached SpiceDB, want 2", n)
		}
	})

	t.Run("ST-SUP-RP11-LC06-T02 A missing row and a denied row cost the same calls and give the same error", func(t *testing.T) {
		e := newEnv(t)
		const allowed = "0000000a-0000-7000-8000-000000000001"
		const denied = "0000000a-0000-7000-8000-000000000002"
		const missing = "0000000a-0000-7000-8000-000000000003"
		e.insert(t, "INSERT INTO app.tickets (tenant_id, id) VALUES ('t1', $1), ('t1', $2)", allowed, denied)
		port := &fakeDecision{delay: 30 * time.Millisecond, decide: func(r authz.EvaluationRequest) bool { return keyOf(r.Resource) == allowed }}
		rt := e.runtimeOf(t, port)
		tickets := ticketEntity(t)
		q := mustQuery(t, "ticket-detail", tickets)
		// get reads each ID through Get in one query and gives the error and the decision port calls of each read.
		get := func(ids ...string) ([]error, []int, []Loaded[Ticket]) {
			errs, calls, values := make([]error, len(ids)), make([]int, len(ids)), make([]Loaded[Ticket], len(ids))
			_, err := Run(context.Background(), rt, q, bound(t, "t1", "user:u-7", "ticket-detail"), struct{}{},
				func(ctx context.Context, _ struct{}, h *Handle[noProbe]) (struct{}, error) {
					repo, err := Repo(h, tickets)
					if err != nil {
						return struct{}{}, err
					}
					for i, id := range ids {
						before := len(port.recorded())
						values[i], errs[i] = repo.Get(ctx, id)
						calls[i] = len(port.recorded()) - before
					}
					return struct{}{}, nil
				})
			if err != nil {
				t.Fatal(err)
			}
			return errs, calls, values
		}
		errs, calls, _ := get(denied, missing)
		for i, name := range []string{"denied", "missing"} {
			if !errors.Is(errs[i], ErrNotFound) || calls[i] != 1 {
				t.Errorf("the %s row gives %v after %d provider calls, want ErrNotFound after 1", name, errs[i], calls[i])
			}
		}
		// A read by the upper-case form of a UUID is decided on the stored lower-case key.
		before := len(port.recorded())
		errs, _, values := get(strings.ToUpper(allowed))
		if errs[0] != nil || values[0].Value().ID != allowed {
			t.Errorf("the read by the upper-case UUID gives %v, %v; want the row %s", values[0].Value(), errs[0], allowed)
		}
		if calls := port.recorded()[before:]; len(calls) != 1 || keyOf(calls[0].Resource) != allowed {
			t.Errorf("the decision is on %v, want one call on the stored key %s", calls, allowed)
		}
		port.mu.Lock()
		port.down = true
		port.mu.Unlock()
		errs, _, _ = get(denied, missing)
		if !errors.Is(errs[0], ErrUndecided) || errs[0] == nil || errs[1] == nil || errs[0].Error() != errs[1].Error() {
			t.Errorf("with the provider down the denied and the missing row give %v and %v, want the same ErrUndecided", errs[0], errs[1])
		}
	})

	t.Run("ST-SUP-RP11-LC07-T02 One list call reads at most 1,000 rows and goes on with a cursor", func(t *testing.T) {
		e := newEnv(t)
		e.insert(t, "INSERT INTO app.tickets (tenant_id, id) SELECT 't1', lpad(to_hex(n), 32, '0')::uuid FROM generate_series(1, 3000) AS n")
		const last = "00000000-0000-0000-0000-000000000bb8"
		port := &fakeDecision{decide: func(r authz.EvaluationRequest) bool { return keyOf(r.Resource) == last }}
		rt := e.runtimeOf(t, port)
		tickets := ticketEntity(t)
		type call struct {
			read, items int
			more        bool
		}
		calls, err := Run(context.Background(), rt, mustQuery(t, "tickets", tickets), bound(t, "t1", "user:u-7", "tickets"), struct{}{},
			func(ctx context.Context, _ struct{}, h *Handle[noProbe]) ([]call, error) {
				repo, err := Repo(h, tickets)
				if err != nil {
					return nil, err
				}
				var calls []call
				var after Cursor
				for len(calls) < 10 {
					before := len(port.recorded())
					page, err := repo.List(ctx, ListRequest{Limit: 1, After: after})
					if err != nil {
						return nil, err
					}
					// The padding decision of a call that read no row is on no row of the table (ST-SUP-RP11-LC20).
					read := 0
					for _, r := range port.recorded()[before:] {
						if !strings.HasSuffix(keyOf(r.Resource), padKey) {
							read++
						}
					}
					c := call{read: read, items: len(page.Items), more: page.More}
					for _, item := range page.Items {
						if item.ID() != last {
							return nil, errors.New("the list holds a row that is not allowed")
						}
					}
					calls = append(calls, c)
					if !page.More {
						return calls, nil
					}
					after = page.Next
				}
				return nil, errors.New("the list does not end")
			})
		if err != nil {
			t.Fatal(err)
		}
		items, read := 0, 0
		for i, c := range calls {
			items += c.items
			read += c.read
			if c.read > ScanLimit {
				t.Errorf("call %d decided %d rows, want at most %d", i, c.read, ScanLimit)
			}
			if i < len(calls)-1 && !c.more {
				t.Errorf("call %d ends the list while rows remain", i)
			}
		}
		if items != 1 || read != 3000 || len(calls) < 3 {
			t.Errorf("the calls %+v give %d items after %d rows, want the allowed row once after 3,000 rows", calls, items, read)
		}
	})

	t.Run("ST-SUP-RP11-LC07-T01 A query list holds only the allowed rows, at one token", func(t *testing.T) {
		e := newEnv(t)
		for i := 1; i <= 5; i++ {
			e.insert(t, "INSERT INTO app.orders (tenant_id, id, total, authz_token) VALUES ('t1', $1, $2, $3)",
				fmt.Sprintf("o-%d", i), i*100, fmt.Sprintf("row-%d", i))
		}
		e.insert(t, "UPDATE authz.authz_floor SET token = 'f1'")
		port := &fakeDecision{decide: func(r authz.EvaluationRequest) bool {
			return keyOf(r.Resource) == "o-2" || keyOf(r.Resource) == "o-4"
		}}
		rt := e.runtimeOf(t, port)
		orders := orderEntity(t, false)
		charges := chargeEntity(t)
		ids := listAll(t, rt, orders, mustListQuery(t, "orders", orders, "list_orders", charges), 1)
		if strings.Join(ids, ",") != "o-2,o-4" {
			t.Errorf("the pages hold %v, want o-2 and o-4", ids)
		}
		port.mu.Lock()
		batches := port.batches
		port.mu.Unlock()
		if len(batches) < 3 {
			t.Fatalf("%d batch evaluations, want one per call of the three calls, at least 3", len(batches))
		}
		for i, batch := range batches {
			for _, r := range batch {
				if got := consistencyOf(r.Consistency); got != "at_least:f1" {
					t.Errorf("page %d decides %s at %s, want at_least:f1", i, r.Resource, got)
				}
				if r.Action.Name != "list_orders" || r.Subject.String() != "user:u-7" {
					t.Errorf("page %d decides %s as %s, want list_orders as user:u-7", i, r.Action.Name, r.Subject)
				}
			}
		}
		// The query's permission is bound to the entity it lists: a list of another entity that the query reads
		// decides the permission that this entity declares for list.
		e.insert(t, "INSERT INTO app.charges (tenant_id, id, card_last4, amount) VALUES ('t1', 'ch-1', '4242', 1)")
		port.mu.Lock()
		port.batches = nil
		port.mu.Unlock()
		_, err := Run(context.Background(), rt, mustListQuery(t, "orders", orders, "list_orders", charges), bound(t, "t1", "user:u-7", "orders"), struct{}{},
			func(ctx context.Context, _ struct{}, h *Handle[noProbe]) (struct{}, error) {
				repo, err := Repo(h, charges)
				if err != nil {
					return struct{}{}, err
				}
				_, err = repo.List(ctx, ListRequest{})
				return struct{}{}, err
			})
		if err != nil {
			t.Fatal(err)
		}
		port.mu.Lock()
		batches = port.batches
		port.mu.Unlock()
		if len(batches) != 1 || len(batches[0]) != 1 || batches[0][0].Action.Name != "view" {
			t.Errorf("the list of charges decides %v, want one batch on the charge's own list permission view", batches)
		}
	})

	t.Run("ST-SUP-RP11-LC07-T01 A query list holds only the allowed rows, decided by SpiceDB", func(t *testing.T) {
		spice := startSpiceDB(t)
		spice.grant(t, "o-1", "u-7")
		floor := spice.grant(t, "o-4", "u-7")
		e := newEnv(t)
		for i := 1; i <= 5; i++ {
			e.insert(t, "INSERT INTO app.orders (tenant_id, id, total) VALUES ('t1', $1, $2)", fmt.Sprintf("o-%d", i), i)
		}
		e.insert(t, "UPDATE authz.authz_floor SET token = $1", floor)
		for _, search := range []bool{false, true} {
			port := &recordingDecision{Decision: spice.provider}
			rt := e.runtimeOf(t, port)
			orders := orderEntity(t, search)
			ids := listAll(t, rt, orders, mustQuery(t, "orders", orders), 1)
			// A list by resource search keeps the provider's order across pages, which is the order of the
			// tenant-qualified IDs (ST-SUP-RP06-LC22), not of the keys.
			sort.Strings(ids)
			if strings.Join(ids, ",") != "o-1,o-4" {
				t.Errorf("with search %v the pages hold %v, want o-1 and o-4", search, ids)
			}
			for _, c := range port.consistencies() {
				if c != "at_least:"+floor {
					t.Errorf("with search %v a page decides at %s, want at_least of the floor", search, c)
				}
			}
			if len(port.consistencies()) == 0 {
				t.Errorf("with search %v no page reached SpiceDB", search)
			}
		}
	})
}

// listAll lists the orders of t1 as user:u-7 through Run, page by page with the limit, and gives their IDs.
func listAll(t *testing.T, rt *Runtime, orders *Entity[Order], q *Query, limit int) []string {
	t.Helper()
	ids, err := Run(context.Background(), rt, q, bound(t, "t1", "user:u-7", q.Name()), struct{}{},
		func(ctx context.Context, _ struct{}, h *Handle[noProbe]) ([]string, error) {
			repo, err := Repo(h, orders)
			if err != nil {
				return nil, err
			}
			var ids []string
			var after Cursor
			for pages := 0; pages < 20; pages++ {
				page, err := repo.List(ctx, ListRequest{Limit: limit, After: after})
				if err != nil {
					return nil, err
				}
				for _, item := range page.Items {
					ids = append(ids, item.ID())
				}
				if !page.More {
					return ids, nil
				}
				after = page.Next
			}
			return nil, errors.New("the list does not end")
		})
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

// recordingDecision records each request that reaches the provider it wraps.
type recordingDecision struct {
	authz.Decision
	mu   sync.Mutex
	seen []authz.Consistency
}

func (r *recordingDecision) note(c authz.Consistency) {
	r.mu.Lock()
	r.seen = append(r.seen, c)
	r.mu.Unlock()
}

func (r *recordingDecision) Evaluate(ctx context.Context, request authz.EvaluationRequest) (authz.EvaluationResult, error) {
	r.note(request.Consistency)
	return r.Decision.Evaluate(ctx, request)
}

func (r *recordingDecision) EvaluateBatch(ctx context.Context, request authz.BatchRequest) ([]authz.EvaluationResult, error) {
	for _, e := range request.Evaluations {
		r.note(e.Consistency)
	}
	return r.Decision.EvaluateBatch(ctx, request)
}

func (r *recordingDecision) Search(ctx context.Context, request authz.SearchRequest) (authz.SearchResult, error) {
	r.note(request.Consistency)
	return r.Decision.Search(ctx, request)
}

func (r *recordingDecision) recorded() []authz.Consistency {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]authz.Consistency(nil), r.seen...)
}

func (r *recordingDecision) consistencies() []string {
	var out []string
	for _, c := range r.recorded() {
		out = append(out, consistencyOf(c))
	}
	return out
}

// spiceDBImage is the pinned SpiceDB image of the local stand-in (ST-STD-LOCAL-R04): version 1.57.0 at its digest,
// as the authorization runtime's tests pin it.
const spiceDBImage = "authzed/spicedb:v1.57.0@sha256:a170d372c9f6b921bb6849064df6ca3566dd5b174882c1a2c4300c2ae1d60efd"

const spiceSchema = `definition user {}

definition order {
	relation viewer: user
	permission view = viewer
}
`

var spiceServer struct {
	once     sync.Once
	name     string
	endpoint string
	err      error
}

// stopSpiceDB removes the SpiceDB container, when a test started one.
func stopSpiceDB() {
	if spiceServer.name != "" {
		_ = exec.Command("docker", "rm", "-f", spiceServer.name).Run()
	}
}

type bearer string

func (b bearer) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(b)}, nil
}

func (bearer) RequireTransportSecurity() bool { return false }

// spiceEnv is the datastore of one test on the local SpiceDB server.
type spiceEnv struct {
	provider *authz.SpiceDB
	perms    v1.PermissionsServiceClient
}

// startSpiceDB gives the test its own datastore, loaded with spiceSchema, on one local serve-testing container
// without privilege, which keeps an in-memory datastore per preshared key. It fails the test when docker is not on
// the PATH, as pgtest does: a missing result is not a pass.
func startSpiceDB(t *testing.T) *spiceEnv {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		if os.Getenv(pgtest.NoContainersVariable) == "1" {
			t.Skip("docker is not on the PATH, and STEGO_TEST_NO_CONTAINERS=1: this test needs a local SpiceDB container")
		}
		t.Fatal("docker is not on the PATH: this test needs a local SpiceDB container")
	}
	spiceServer.once.Do(func() {
		suffix := make([]byte, 6)
		if _, err := rand.Read(suffix); err != nil {
			spiceServer.err = err
			return
		}
		name := "stego-query-spicedb-" + hex.EncodeToString(suffix)
		run := exec.Command("docker", "run", "-d", "--rm", "--name", name, "--memory", "256m", "--cpus", "1",
			"--cap-drop", "ALL", "--security-opt", "no-new-privileges", "-p", "127.0.0.1::50051", spiceDBImage, "serve-testing")
		var stderr bytes.Buffer
		run.Stderr = &stderr
		if err := run.Run(); err != nil {
			spiceServer.err = fmt.Errorf("start the SpiceDB container: %v: %s", err, stderr.String())
			return
		}
		spiceServer.name = name
		out, err := exec.Command("docker", "port", name, "50051/tcp").Output()
		if err != nil {
			spiceServer.err = fmt.Errorf("read the port of the SpiceDB container: %v", err)
			return
		}
		spiceServer.endpoint = strings.TrimSpace(strings.Split(string(out), "\n")[0])
	})
	if spiceServer.err != nil {
		t.Fatal(spiceServer.err)
	}
	key := make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	conn, err := grpc.NewClient(spiceServer.endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(bearer(hex.EncodeToString(key))))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	schema := v1.NewSchemaServiceClient(conn)
	deadline := time.Now().Add(60 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err = schema.WriteSchema(ctx, &v1.WriteSchemaRequest{Schema: spiceSchema})
		cancel()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("write the schema to the SpiceDB container: %v", err)
		}
		time.Sleep(250 * time.Millisecond)
	}
	provider, err := authz.NewSpiceDB(conn)
	if err != nil {
		t.Fatal(err)
	}
	return &spiceEnv{provider: provider, perms: v1.NewPermissionsServiceClient(conn)}
}

// grant makes the user a viewer of the order and gives the ZedToken of the write.
func (s *spiceEnv) grant(t *testing.T, order, user string) string {
	t.Helper()
	// The order is of tenant t1, in the tenant-qualified form that the runtime sends (ST-SUP-RP06-LC22).
	response, err := s.perms.WriteRelationships(context.Background(), &v1.WriteRelationshipsRequest{Updates: []*v1.RelationshipUpdate{{
		Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
		Relationship: &v1.Relationship{
			Resource: &v1.ObjectReference{ObjectType: "order", ObjectId: resourceIn(t, "t1", "order", order).ID},
			Relation: "viewer",
			Subject:  &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: user}},
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return response.GetWrittenAt().GetToken()
}
