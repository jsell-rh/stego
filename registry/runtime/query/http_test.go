package query

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jsell-rh/stego/registry/runtime/audit"
	"github.com/jsell-rh/stego/registry/runtime/authz"
	"github.com/jsell-rh/stego/registry/runtime/internal/seal"
	"github.com/jsell-rh/stego/registry/runtime/internal/staging"
	"github.com/jsell-rh/stego/registry/runtime/probe"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
)

// testCursorKey is the cursor key of the tests: 32 fixed bytes.
var testCursorKey = []byte("0123456789abcdef0123456789abcdef")

// servedRuntime is the query runtime of the env with the decision port provider and the cursor key.
func (e *env) servedRuntime(t *testing.T, provider authz.Decision) *Runtime {
	t.Helper()
	az, err := authz.New(authz.Config{Provider: provider, DB: e.db, Schema: "authz", Outbox: e.queue})
	if err != nil {
		t.Fatal(err)
	}
	rt, err := New(Config{DB: e.db, AuditDB: e.auditDB, Audit: e.writer, AuditSettings: audit.Settings{Level: audit.LevelChange},
		Authz: az, Service: "shop", CursorKey: testCursorKey})
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

// headerBind is the Bind of the tests: the tenant and the subject come from two headers, as the generated code takes
// them from the verified identity, and the probe call is bound to them and to the query name.
func headerBind(q *Query) func(r *http.Request) (Bound[noProbe], error) {
	return func(r *http.Request) (Bound[noProbe], error) {
		id := r.Header.Get("X-Tenant")
		tn, err := tenant.New(tenant.Requested(tenant.ProvisionAction, id, decided{seal.NewDecisionMark(id, "provision")}))
		if err != nil {
			return Bound[noProbe]{}, err
		}
		p, err := authz.StagingPrincipal(staging.NewStaging(), r.Header.Get("X-Subject"))
		if err != nil {
			return Bound[noProbe]{}, err
		}
		call := probe.NewRecorder(nil).Bind(r.Context(), tn.ID(), p.Subject().String(), q.Name())
		return Bound[noProbe]{Tenant: tn, Principal: p, Call: call, Probe: newNoProbe}, nil
	}
}

// orderPage is the response of the list query of the tests.
type orderPage struct {
	IDs  []string
	next Cursor
}

// listOrders serves the list query q over orders, two rows a page; fills counts the calls of the fill.
func listOrders(t *testing.T, rt *Runtime, q *Query, orders *Entity[Order], fills *atomic.Int64) http.Handler {
	t.Helper()
	h, err := Serve(rt, HTTP[ListRequest, orderPage, noProbe]{
		Query: q,
		Bind:  headerBind(q),
		Decode: func(_ *http.Request, after Cursor) (ListRequest, error) {
			return ListRequest{Limit: 2, After: after}, nil
		},
		Fill: func(ctx context.Context, req ListRequest, h *Handle[noProbe]) (orderPage, error) {
			fills.Add(1)
			repo, err := Repo(h, orders)
			if err != nil {
				return orderPage{}, err
			}
			page, err := repo.List(ctx, req)
			if err != nil {
				return orderPage{}, err
			}
			out := orderPage{IDs: []string{}}
			for _, o := range page.Items {
				out.IDs = append(out.IDs, o.Value().ID)
			}
			if page.More {
				out.next = page.Next
			}
			return out, nil
		},
		Next: func(res orderPage) Cursor { return res.next },
		Encode: func(res orderPage, next string) ([]byte, error) {
			return json.Marshal(map[string]any{"ids": res.IDs, "next": next})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

type served struct {
	status      int
	cache       string
	contentType string
	body        string
	IDs         []string `json:"ids"`
	Next        string   `json:"next"`
}

// get sends a GET of path to h as the tenant and the subject.
func get(t *testing.T, h http.Handler, path, tenantID, subject string) served {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set("X-Tenant", tenantID)
	r.Header.Set("X-Subject", subject)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	body, _ := io.ReadAll(w.Result().Body)
	s := served{status: w.Code, cache: w.Header().Get("Cache-Control"), contentType: w.Header().Get("Content-Type"), body: string(body)}
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(body, &s); err != nil {
			t.Fatalf("the body %q does not decode: %v", body, err)
		}
	}
	return s
}

// wantProblem checks that s is the problem response of status, with the fixed fields only.
func wantProblem(t *testing.T, s served, status int, forbidden ...string) {
	t.Helper()
	if s.status != status || s.contentType != "application/problem+json" || s.cache != "no-store" {
		t.Fatalf("the response is %d %s %s, want the problem response %d", s.status, s.contentType, s.body, status)
	}
	var p map[string]any
	if err := json.Unmarshal([]byte(s.body), &p); err != nil {
		t.Fatalf("the problem %q does not decode: %v", s.body, err)
	}
	if len(p) != 3 || p["type"] != "about:blank" || p["status"] != float64(status) || p["title"] == "" {
		t.Fatalf("the problem is %v, want only type, title and status", p)
	}
	for _, f := range forbidden {
		if strings.Contains(s.body, f) {
			t.Fatalf("the problem %q holds %q", s.body, f)
		}
	}
}

func cursorPath(text string) string {
	return "/orders?" + CursorParameter + "=" + url.QueryEscape(text)
}

// wantSealedOnly decodes the cursor text and checks, with no search in random bytes, that it is exactly the cursor
// version, a nonce of the AEAD's size, and the AEAD seal of position under that nonce with the associated data of
// the query, the tenant and the principal. So the text holds no other byte, and the position shows only as its
// seal. It gives the decoded bytes.
func wantSealedOnly(t *testing.T, rt *Runtime, q *Query, tn, subject, text string, position []byte) []byte {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(text)
	if err != nil {
		t.Fatalf("the cursor %q does not decode: %v", text, err)
	}
	nonceSize := rt.cursorAEAD.NonceSize()
	size := 1 + nonceSize + len(position) + rt.cursorAEAD.Overhead()
	if len(raw) != size || raw[0] != cursorVersion {
		t.Fatalf("the cursor %q has %d bytes, want version %d and %d bytes", text, len(raw), cursorVersion, size)
	}
	nonce := raw[1 : 1+nonceSize]
	want := rt.cursorAEAD.Seal(nil, nonce, position, rt.cursorData(q, mustTenant(t, tn), mustPrincipal(t, subject)))
	if !bytes.Equal(raw[1+nonceSize:], want) {
		t.Fatalf("the cursor %q is not the seal of its position %q", text, position)
	}
	return raw
}

func TestCursor(t *testing.T) {
	t.Run("ST-SUP-RP11-LC15-T01 A changed or moved cursor is refused", func(t *testing.T) {
		e := newEnv(t)
		decision := &fakeDecision{decide: allowAll}
		rt := e.servedRuntime(t, decision)
		orders := orderEntity(t, false)
		for _, id := range []string{"o1", "o2", "o3", "o4", "o5"} {
			e.insert(t, "INSERT INTO app.orders (tenant_id, id, total) VALUES ('t1', $1, 10), ('t2', $1, 20)", id)
		}
		q := mustListQuery(t, "list-orders", orders, "view")
		other := mustListQuery(t, "list-orders-again", orders, "view")
		var fills atomic.Int64
		h := listOrders(t, rt, q, orders, &fills)
		hOther := listOrders(t, rt, other, orders, &fills)

		first := get(t, h, "/orders", "t1", "user:u-7")
		if first.status != http.StatusOK || strings.Join(first.IDs, ",") != "o1,o2" || first.Next == "" {
			t.Fatalf("the first page is %d %v %q, want o1 and o2 with a cursor", first.status, first.IDs, first.Next)
		}
		// The position of the first page is the key o2. The text holds only the version, the nonce and the
		// AEAD seal of that position, so it shows no key, query or principal.
		raw := wantSealedOnly(t, rt, q, "t1", "user:u-7", first.Next, appendText([]byte{cursorVersion, cursorTable}, "o2"))
		second := get(t, h, cursorPath(first.Next), "t1", "user:u-7")
		if second.status != http.StatusOK || strings.Join(second.IDs, ",") != "o3,o4" {
			t.Fatalf("the unchanged cursor gave %d %v, want o3 and o4", second.status, second.IDs)
		}

		// One byte of the sealed position changed, and one byte of the tag.
		changed := append([]byte(nil), raw...)
		changed[len(changed)-33] ^= 0x01
		flipped := base64.RawURLEncoding.EncodeToString(changed)
		macChanged := append([]byte(nil), raw...)
		macChanged[len(macChanged)-1] ^= 0x80
		fillsBefore, callsBefore := fills.Load(), len(decision.recorded())
		for name, c := range map[string]struct {
			h                 http.Handler
			text, tn, subject string
		}{
			"a byte of the position changed": {h, flipped, "t1", "user:u-7"},
			"a byte of the tag changed":      {h, base64.RawURLEncoding.EncodeToString(macChanged), "t1", "user:u-7"},
			"another query":                  {hOther, first.Next, "t1", "user:u-7"},
			"another tenant":                 {h, first.Next, "t2", "user:u-7"},
			"another principal":              {h, first.Next, "t1", "user:u-9"},
			"text that does not decode":      {h, "not a cursor!", "t1", "user:u-7"},
		} {
			wantProblem(t, get(t, c.h, cursorPath(c.text), c.tn, c.subject), http.StatusBadRequest, "o2", "o3")
			if fills.Load() != fillsBefore || len(decision.recorded()) != callsBefore {
				t.Fatalf("%s reached the fill or the decision port: no row may be read", name)
			}
		}
		if _, err := rt.ParseCursor(q, mustTenant(t, "t2"), mustPrincipal(t, "user:u-7"), first.Next); !errors.Is(err, ErrBadCursor) {
			t.Fatalf("ParseCursor for another tenant gave %v, want ErrBadCursor", err)
		}

		// A search cursor keeps the provider's page token and the first page's consistency.
		token, _ := authz.NewToken("zed-7")
		search := Cursor{page: authz.NewPageToken("spicedb", "page-2", authz.AtLeast(token))}
		text, err := rt.CursorText(q, mustTenant(t, "t1"), mustPrincipal(t, "user:u-7"), search)
		if err != nil {
			t.Fatal(err)
		}
		// The search position holds the provider spicedb, its cursor page-2 and the token zed-7; the text holds
		// only their seal.
		searchPosition := appendText([]byte{cursorVersion, cursorSearch}, "spicedb")
		searchPosition = appendText(searchPosition, "page-2")
		searchPosition = appendText(append(searchPosition, consistencyAtLeast), token.Text())
		wantSealedOnly(t, rt, q, "t1", "user:u-7", text, searchPosition)
		// The same key in another service opens no cursor of this one.
		otherService, err := New(Config{DB: e.db, Service: "billing", CursorKey: testCursorKey})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := otherService.ParseCursor(q, mustTenant(t, "t1"), mustPrincipal(t, "user:u-7"), text); !errors.Is(err, ErrBadCursor) {
			t.Fatalf("another service opened the cursor: %v", err)
		}
		back, err := rt.ParseCursor(q, mustTenant(t, "t1"), mustPrincipal(t, "user:u-7"), text)
		if err != nil || back != search {
			t.Fatalf("the search cursor came back as %+v, %v", back, err)
		}
		if _, err := New(Config{DB: e.db, Service: "shop", CursorKey: []byte("short")}); err == nil {
			t.Fatal("a cursor key of 5 bytes was accepted")
		}
		if _, err := New(Config{DB: e.db, CursorKey: testCursorKey}); err == nil {
			t.Fatal("a cursor key with no service name was accepted")
		}
	})
}

func TestServe(t *testing.T) {
	t.Run("ST-SUP-RP11-LC17-T01 One helper serves get and list", func(t *testing.T) {
		e := newEnv(t)
		decision := &fakeDecision{decide: func(r authz.EvaluationRequest) bool { return keyOf(r.Resource) != "o2" }}
		rt := e.servedRuntime(t, decision)
		orders := orderEntity(t, false)
		e.insert(t, "INSERT INTO app.orders (tenant_id, id, total) VALUES ('t1', 'o1', 4711), ('t1', 'o2', 4712), ('t1', 'o3', 4713)")
		getOrder := mustQuery(t, "get-order", orders)
		h, err := Serve(rt, HTTP[string, Order, noProbe]{
			Query: getOrder,
			Bind:  headerBind(getOrder),
			Decode: func(r *http.Request, _ Cursor) (string, error) {
				id := r.URL.Query().Get("id")
				if id == "" {
					return "", ErrBadRequest
				}
				return id, nil
			},
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
			Encode: func(o Order, _ string) ([]byte, error) {
				return json.Marshal(map[string]any{"id": o.ID, "total": o.Total})
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		ok := get(t, h, "/order?id=o1", "t1", "user:u-7")
		if ok.status != http.StatusOK || ok.contentType != "application/json" || ok.body != `{"id":"o1","total":4711}` {
			t.Fatalf("the get gave %d %s %s, want 200 with the row", ok.status, ok.contentType, ok.body)
		}
		wantProblem(t, get(t, h, "/order?id=o2", "t1", "user:u-7"), http.StatusNotFound, "o2", "4712")
		wantProblem(t, get(t, h, "/order?id=o9", "t1", "user:u-7"), http.StatusNotFound, "o9")
		wantProblem(t, get(t, h, "/order", "t1", "user:u-7"), http.StatusBadRequest)
		wantProblem(t, get(t, h, "/order?id=o1", "", ""), http.StatusUnauthorized)

		var fills atomic.Int64
		list := listOrders(t, rt, mustListQuery(t, "list-orders", orders, "view"), orders, &fills)
		page := get(t, list, "/orders", "t1", "user:u-7")
		if page.status != http.StatusOK || strings.Join(page.IDs, ",") != "o1,o3" {
			t.Fatalf("the list gave %d %v, want the allowed rows o1 and o3", page.status, page.IDs)
		}
		wantProblem(t, get(t, list, cursorPath("garbage"), "t1", "user:u-7"), http.StatusBadRequest)
		if fills.Load() != 1 {
			t.Fatalf("the bad cursor reached the fill")
		}
		if page.cache != "no-store" {
			t.Fatalf("the list response has Cache-Control %q, want no-store", page.cache)
		}
		// A provider that fails is a read that could not be decided: 503, not 500.
		decision.mu.Lock()
		decision.down = true
		decision.mu.Unlock()
		wantProblem(t, get(t, list, "/orders", "t1", "user:u-7"), http.StatusServiceUnavailable, "the provider is down")
		decision.mu.Lock()
		decision.down = false
		decision.mu.Unlock()

		// Decode reads at most MaxRequestBody bytes of the body.
		var read atomic.Int64
		bodyQuery := mustQuery(t, "body-order", orders)
		withBody, err := Serve(rt, HTTP[string, Order, noProbe]{Query: bodyQuery, Bind: headerBind(bodyQuery),
			Decode: func(r *http.Request, _ Cursor) (string, error) {
				n, err := io.Copy(io.Discard, r.Body)
				read.Store(n)
				if err != nil {
					return "", ErrBadRequest
				}
				return "o1", nil
			},
			Fill:   func(context.Context, string, *Handle[noProbe]) (Order, error) { return Order{ID: "o1"}, nil },
			Encode: func(Order, string) ([]byte, error) { return []byte(`{}`), nil }})
		if err != nil {
			t.Fatal(err)
		}
		big := httptest.NewRequest(http.MethodGet, "/order", strings.NewReader(strings.Repeat("x", MaxRequestBody+1)))
		big.Header.Set("X-Tenant", "t1")
		big.Header.Set("X-Subject", "user:u-7")
		w := httptest.NewRecorder()
		withBody.ServeHTTP(w, big)
		if w.Code != http.StatusBadRequest || read.Load() > MaxRequestBody {
			t.Fatalf("a body above the limit gave %d after %d bytes read, want 400 within the limit", w.Code, read.Load())
		}

		r := httptest.NewRequest(http.MethodPost, "/orders", nil)
		w = httptest.NewRecorder()
		list.ServeHTTP(w, r)
		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("a POST gave %d, want 405", w.Code)
		}
		noKey := e.runtimeOf(t, decision)
		if _, err := Serve(noKey, HTTP[ListRequest, orderPage, noProbe]{Query: getOrder, Bind: headerBind(getOrder),
			Decode: func(*http.Request, Cursor) (ListRequest, error) { return ListRequest{}, nil },
			Fill:   func(context.Context, ListRequest, *Handle[noProbe]) (orderPage, error) { return orderPage{}, nil },
			Next:   func(orderPage) Cursor { return Cursor{} },
			Encode: func(orderPage, string) ([]byte, error) { return nil, nil }}); !errors.Is(err, ErrNoCursorKey) {
			t.Fatalf("a list helper with no cursor key gave %v, want ErrNoCursorKey", err)
		}
	})
}
