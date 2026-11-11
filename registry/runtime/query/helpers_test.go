package query

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jsell-rh/stego/registry/runtime/authn"
	"github.com/jsell-rh/stego/registry/runtime/authn/authntest"
	"github.com/jsell-rh/stego/registry/runtime/otel"
	"github.com/jsell-rh/stego/registry/runtime/probe"
)

// codedGet sends a GET of path to h as the tenant and the subject, with a place for the fixed error code of the
// call, as the telemetry runtime's Handler gives it, and returns the response and the code.
func codedGet(t *testing.T, h http.Handler, path, tenantID, subject string) (served, string) {
	t.Helper()
	var code string
	s := get(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := otel.WithErrorCode(r.Context())
		h.ServeHTTP(w, r.WithContext(ctx))
		code, _ = otel.ErrorCode(ctx)
	}), path, tenantID, subject)
	return s, code
}

func TestServeHelpers(t *testing.T) {
	t.Run("ST-SUP-RP11-LC17-T01 The serving helpers of the runtime: refusals, bind, limit, page and row reads", func(t *testing.T) {
		e := newEnv(t)
		rt := e.servedRuntime(t, &fakeDecision{decide: allowAll})
		orders := orderEntity(t, false)
		e.insert(t, "INSERT INTO app.orders (tenant_id, id, total) VALUES ('t1', 'o1', 4711)")
		getOrder := mustQuery(t, "get-order", orders)
		unknown := NewRefusal("get-order", "order_unknown", http.StatusNotFound, "order.unknown")
		other := NewRefusal("list-orders", "order_unknown", http.StatusConflict, "order.other")
		var failWith error
		serve := func(lookup func(error) (int, string, bool)) http.Handler {
			h, err := Serve(rt, HTTP[string, Order, noProbe]{Query: getOrder, Bind: headerBind(getOrder),
				Decode: func(r *http.Request, _ Cursor) (string, error) { return r.URL.Query().Get("id"), nil },
				Fill: func(ctx context.Context, id string, h *Handle[noProbe]) (Order, error) {
					repo, err := Repo(h, orders)
					if err != nil {
						return Order{}, err
					}
					if _, err := repo.Get(ctx, id); err != nil {
						return Order{}, err
					}
					return Order{}, failWith
				},
				Encode: EncodeValue[Order], Refusal: lookup})
			if err != nil {
				t.Fatal(err)
			}
			return h
		}
		h := serve(Lookup(unknown))
		// A declared refusal is answered with its status and code, which the completion log gets too.
		failWith = unknown
		s, code := codedGet(t, h, "/order?id=o1", "t1", "user:u-7")
		var p map[string]any
		_ = json.Unmarshal([]byte(s.body), &p)
		if s.status != http.StatusNotFound || p["code"] != "order.unknown" || code != "order.unknown" || len(p) != 4 {
			t.Fatalf("the refusal gave %d %s with the log code %q, want 404 with the code order.unknown", s.status, s.body, code)
		}
		// Another error, and a refusal that the lookup does not know, are 500 internal with no error text.
		for name, err := range map[string]error{
			"an error whose text holds an email":   errors.New("no order for jane.doe@example.com"),
			"a refusal of another query":           other,
			"a refusal that the fill made itself": NewRefusal("get-order", "order_unknown", http.StatusNotFound, "order.unknown"),
		} {
			failWith = err
			s, code := codedGet(t, h, "/order?id=o1", "t1", "user:u-7")
			p = nil
			_ = json.Unmarshal([]byte(s.body), &p)
			if s.status != http.StatusInternalServerError || p["code"] != ErrorCodeInternal || code != ErrorCodeInternal ||
				strings.Contains(s.body, "example.com") {
				t.Fatalf("%s gave %d %s with the log code %q, want 500 internal with no error text", name, s.status, s.body, code)
			}
		}
		// With no lookup, a declared refusal is 500 internal too.
		failWith = unknown
		if s, code := codedGet(t, serve(nil), "/order?id=o1", "t1", "user:u-7"); s.status != http.StatusInternalServerError || code != ErrorCodeInternal {
			t.Fatalf("a refusal with no lookup gave %d with the log code %q, want 500 internal", s.status, code)
		}

		// BindHTTP binds the verified identity: the tenant of its claim, its subject and the probe call of the query.
		key := authntest.NewKey(t, "k1")
		set, err := authn.ParseKeys(authntest.Document(key.JWK()))
		if err != nil {
			t.Fatal(err)
		}
		verifier, err := authn.New(authn.Settings{Issuer: "https://issuer.example", Audience: "shop", ProblemCode: "SHOP",
			Keys: set, TenantClaim: "tenant"})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(verifier.Stop)
		recorder := probe.NewRecorder(nil)
		var got Bound[noProbe]
		bindQuery := mustQuery(t, "bind-order", orders)
		bound, err := Serve(rt, HTTP[string, Order, noProbe]{Query: bindQuery, Bind: func(r *http.Request) (Bound[noProbe], error) {
			b, err := BindHTTP(recorder, "user", bindQuery, newNoProbe)(r)
			got = b
			return b, err
		},
			Decode: func(*http.Request, Cursor) (string, error) { return "o1", nil },
			Fill: func(ctx context.Context, id string, h *Handle[noProbe]) (Order, error) {
				repo, err := Repo(h, orders)
				if err != nil {
					return Order{}, err
				}
				o, err := repo.Get(ctx, id)
				if err != nil {
					return Order{}, err
				}
				return o.Value(), nil
			},
			Encode: EncodeValue[Order]})
		if err != nil {
			t.Fatal(err)
		}
		claims := authntest.Claims("https://issuer.example", "shop", "u-7", time.Now())
		claims["tenant"] = "t1"
		r := httptest.NewRequest(http.MethodGet, "/order", nil)
		r.Header.Set("Authorization", "Bearer "+authntest.Mint(t, authntest.Header("k1"), claims, key))
		r.Header.Set(RequestIDHeader, "req-1")
		w := httptest.NewRecorder()
		verifier.Middleware(bound).ServeHTTP(w, r)
		if w.Code != http.StatusOK || got.Tenant.ID() != "t1" || got.Principal.Subject().Type != "user" ||
			got.RequestID != "req-1" || got.Call == nil || got.Call.Probe().Operation() != "bind-order" {
			t.Fatalf("the bound call gave %d with %+v, want 200 bound to t1, the user and the query", w.Code, got)
		}
		// A verified identity with no tenant claim is not bound: 401.
		delete(claims, "tenant")
		r = httptest.NewRequest(http.MethodGet, "/order", nil)
		r.Header.Set("Authorization", "Bearer "+authntest.Mint(t, authntest.Header("k1"), claims, key))
		w = httptest.NewRecorder()
		got = Bound[noProbe]{}
		verifier.Middleware(bound).ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("an identity with no tenant claim gave %d, want 401", w.Code)
		}
		// A request with no verified identity is not bound: 401.
		w = httptest.NewRecorder()
		bound.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/order", nil))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("a request with no verified identity gave %d, want 401", w.Code)
		}

		// Limit reads the limit parameter, and EncodePage writes the page body with no total.
		for text, want := range map[string]int{"": 0, "?limit=1": 1, "?limit=1000": 1000} {
			if n, err := Limit(httptest.NewRequest(http.MethodGet, "/orders"+text, nil)); err != nil || n != want {
				t.Fatalf("Limit of %q gave %d, %v, want %d", text, n, err, want)
			}
		}
		for _, text := range []string{"?limit=0", "?limit=1001", "?limit=x", "?limit=-1"} {
			if _, err := Limit(httptest.NewRequest(http.MethodGet, "/orders"+text, nil)); !errors.Is(err, ErrBadRequest) {
				t.Fatalf("Limit of %q gave %v, want ErrBadRequest", text, err)
			}
		}
		if body, err := EncodePage([]string{"o1"}, "c2"); err != nil || string(body) != `{"items":["o1"],"next":"c2"}` {
			t.Fatalf("EncodePage gave %s, %v", body, err)
		}
		if body, err := EncodePage([]string{}, ""); err != nil || string(body) != `{"items":[]}` {
			t.Fatalf("EncodePage at the end gave %s, %v", body, err)
		}

		// The row reads of the column kinds that generated loads read.
		row := Row{columns: []string{"n", "big", "f", "b", "j", "s", "null"},
			values: []any{int64(-7), int64(1) << 40, float64(1.5), []byte{1, 2}, []byte(`{"a":1}`), `{"b":2}`, nil}}
		if n, err := row.Int32("n"); err != nil || n != -7 {
			t.Fatalf("Int32 gave %d, %v", n, err)
		}
		if _, err := row.Int32("big"); err == nil {
			t.Fatal("Int32 took a value out of the int32 range")
		}
		if f, err := row.Float("f"); err != nil || f != 1.5 {
			t.Fatalf("Float gave %v, %v", f, err)
		}
		if b, err := row.Bytes("b"); err != nil || len(b) != 2 || b[1] != 2 {
			t.Fatalf("Bytes gave %v, %v", b, err)
		}
		if b, _ := row.Bytes("b"); true {
			b[0] = 9
			if again, _ := row.Bytes("b"); again[0] != 1 {
				t.Fatal("Bytes gave the stored bytes, not a copy")
			}
		}
		if j, err := row.JSON("j"); err != nil || string(j) != `{"a":1}` {
			t.Fatalf("JSON gave %s, %v", j, err)
		}
		if j, err := row.JSON("s"); err != nil || string(j) != `{"b":2}` {
			t.Fatalf("JSON of a text value gave %s, %v", j, err)
		}
		for _, read := range []func() error{
			func() error { v, err := row.Int32("null"); return errIf(v != 0, err) },
			func() error { v, err := row.Float("null"); return errIf(v != 0, err) },
			func() error { v, err := row.Bytes("null"); return errIf(v != nil, err) },
			func() error { v, err := row.JSON("null"); return errIf(v != nil, err) },
		} {
			if err := read(); err != nil {
				t.Fatalf("a NULL read gave %v", err)
			}
		}
		if _, err := row.Float("s"); err == nil {
			t.Fatal("Float took a text value")
		}
	})
}

// errIf gives err, or an error when bad is true.
func errIf(bad bool, err error) error {
	if bad {
		return errors.New("the value is not the zero value")
	}
	return err
}
