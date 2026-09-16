package keycloak

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	runtime "example.com/provider/out/controller"
)

func TestClientCursorResumesAfterEveryItem(t *testing.T) {
	var reads atomic.Int32
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if authRequest(w, r) {
			return
		}
		reads.Add(1)
		q := r.URL.Query()
		first, _ := strconv.Atoi(q.Get("first"))
		size, _ := strconv.Atoi(q.Get("max"))
		if q.Get("clientId") != "catalog" || q.Get("search") != "true" || size < 1 || size > 2 {
			t.Error("cursor changed the query")
			w.WriteHeader(400)
			return
		}
		values := []ClientRepresentation{}
		for i := first; i < min(first+size, 3); i++ {
			values = append(values, ClientRepresentation{ID: strconv.Itoa(i), ClientID: "catalog-" + strconv.Itoa(i)})
		}
		_ = json.NewEncoder(w).Encode(values)
	})
	source, err := c.ClientNameCursorSource("catalog")
	if err != nil {
		t.Fatal(err)
	}
	page, err := source(context.Background(), "", 2)
	if err != nil || len(page.Items) != 2 || !page.More || page.Items[0].Cursor != "1.1" || page.Items[1].Cursor != "1.2" {
		t.Fatal("first cursor page differs", err)
	}
	next, err := source(context.Background(), page.Items[0].Cursor, 1)
	if err != nil || len(next.Items) != 1 || next.Items[0].Value.ID != "1" {
		t.Fatal("cursor skipped an unprocessed item", err)
	}
	last, err := source(context.Background(), page.Items[1].Cursor, 2)
	if err != nil || len(last.Items) != 1 || last.More || last.Items[0].Cursor != "1.3" {
		t.Fatal("short final page differs", err)
	}
	if reads.Load() != 3 {
		t.Fatal("source repeated a query")
	}
}
func TestClientCursorRejectsInvalidInputAndInventoryLimit(t *testing.T) {
	var requests atomic.Int32
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		t.Error("invalid input reached provider")
		w.WriteHeader(500)
	})
	for _, q := range []string{"", " ", "bad%", "bad_"} {
		if _, err := c.ClientNameCursorSource(q); err == nil {
			t.Fatal("invalid fragment accepted")
		}
	}
	var absent *Client
	if _, err := absent.ClientNameCursorSource("catalog"); err == nil {
		t.Fatal("nil client accepted")
	}
	source, err := c.ClientNameCursorSource("catalog")
	if err != nil {
		t.Fatal(err)
	}
	for _, cursor := range []string{"0", "1.0", "1.01", "1.-1", "1.+1", "2.1", "1.10001", "1.9999999999999999999999"} {
		if _, err := source(context.Background(), cursor, 1); !errors.Is(err, runtime.ErrScanContract) {
			t.Fatal("invalid cursor accepted", err)
		}
	}
	for _, size := range []int{-1, 0, 101} {
		if _, err := source(context.Background(), "", size); !errors.Is(err, runtime.ErrScanContract) {
			t.Fatal("invalid size accepted", err)
		}
	}
	if _, err := source(nil, "", 1); !errors.Is(err, runtime.ErrScanContract) {
		t.Fatal("nil context accepted", err)
	}
	if page, err := source(context.Background(), "1.10000", 1); !errors.Is(err, ErrClientInventoryLimit) || page.More || len(page.Items) != 0 {
		t.Fatal("inventory limit became completion", err)
	}
	if requests.Load() != 0 {
		t.Fatal("invalid input reached network")
	}
}
func TestClientCursorKeepsFailureAcrossRestart(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if authRequest(w, r) {
			return
		}
		first, _ := strconv.Atoi(r.URL.Query().Get("first"))
		if first == 0 {
			_, _ = w.Write([]byte(`[{"id":"first","clientId":"catalog-first"},{"id":"second","clientId":"catalog-second"}]`))
			return
		}
		if first != 2 {
			t.Error("restart lost cursor")
			w.WriteHeader(400)
			return
		}
		_, _ = w.Write([]byte(`[{"id":"third","clientId":"catalog-third"}]`))
	})
	var saved runtime.Checkpoint
	access := runtime.CheckpointAccess{
		Load: func(context.Context) (runtime.Checkpoint, error) { return saved, nil },
		Save: func(_ context.Context, version int64, data string) error {
			if version != saved.Version {
				return errors.New("checkpoint conflict")
			}
			saved = runtime.Checkpoint{Version: version + 1, After: data}
			return nil
		},
	}
	scan := runtime.ScanOptions{PageSize: 2, MaxPages: 1, PageTimeout: time.Second}
	budget := runtime.ObservationOptions{WorkTimeout: time.Second, CommitTimeout: time.Second}
	first, _ := c.ClientNameCursorSource("catalog")
	var visited []string
	state, err := runtime.ScanCycle(context.Background(), "realm-and-query-v1", access, first, func(_ context.Context, v ClientRepresentation) error {
		visited = append(visited, v.ID)
		if v.ID == "first" {
			return errors.New("read failed")
		}
		return nil
	}, func(error) bool { return true }, scan, budget)
	if !errors.Is(err, runtime.ErrCycleFailed) || state.Complete || state.After != "1.2" || len(visited) != 2 {
		t.Fatal("failed item blocked independent progress", err)
	}
	restarted, _ := c.ClientNameCursorSource("catalog")
	state, err = runtime.ScanCycle(context.Background(), "realm-and-query-v1", access, restarted, func(_ context.Context, v ClientRepresentation) error { visited = append(visited, v.ID); return nil }, func(error) bool { return true }, scan, budget)
	if !errors.Is(err, runtime.ErrCycleFailed) || !state.Complete || !state.Failed || len(visited) != 3 || visited[2] != "third" {
		t.Fatal("restart lost failure or later candidate", err)
	}
}

func TestClientCursorSourceVersionBindsProviderAndQuery(t *testing.T) {
	c := &Client{issuer: "https://identity.example/realms/tenant", clientID: "worker"}
	prior, err := c.ClientNameSourceVersion("catalog")
	if err != nil || len(prior) != 64 {
		t.Fatal("invalid source version", err)
	}
	again, err := c.ClientNameSourceVersion("catalog")
	if err != nil || again != prior {
		t.Fatal("unstable source version", err)
	}
	for _, changed := range []*Client{{issuer: c.issuer, clientID: "other"}, {issuer: "https://identity.example/realms/other", clientID: c.clientID}} {
		value, err := changed.ClientNameSourceVersion("catalog")
		if err != nil || value == prior {
			t.Fatal("changed provider kept source version", err)
		}
	}
	value, err := c.ClientNameSourceVersion("other")
	if err != nil || value == prior {
		t.Fatal("changed query kept source version", err)
	}
	if _, err = c.ClientNameSourceVersion("bad%"); err == nil {
		t.Fatal("invalid query accepted")
	}
	var absent *Client
	if _, err = absent.ClientNameSourceVersion("catalog"); err == nil {
		t.Fatal("nil client accepted")
	}
}

func TestClientCursorInventoryLimitRestartsFailedCycle(t *testing.T) {
	var reads atomic.Int32
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if authRequest(w, r) {
			return
		}
		reads.Add(1)
		if r.URL.Query().Get("first") != "0" {
			t.Error("new inventory cycle did not restart")
			w.WriteHeader(400)
			return
		}
		_, _ = w.Write([]byte(`[]`))
	})
	source, err := c.ClientNameCursorSource("catalog")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := runtime.EncodeCycle(runtime.CycleState{Source: "realm-query", After: "1.10000"})
	if err != nil {
		t.Fatal(err)
	}
	saved := runtime.Checkpoint{Version: 1, After: encoded}
	access := runtime.CheckpointAccess{Load: func(context.Context) (runtime.Checkpoint, error) { return saved, nil }, Save: func(_ context.Context, version int64, data string) error {
		if version != saved.Version {
			t.Fatal("stale limit save")
		}
		saved = runtime.Checkpoint{Version: version + 1, After: data}
		return nil
	}}
	scan := runtime.ScanOptions{PageSize: 20, MaxPages: 1, PageTimeout: time.Second}
	budget := runtime.ObservationOptions{WorkTimeout: time.Second, CommitTimeout: time.Second}
	emit := func(context.Context, ClientRepresentation) error {
		t.Fatal("empty fixture emitted a client")
		return nil
	}
	state, err := runtime.ScanCycle(context.Background(), "realm-query", access, source, emit, nil, scan, budget)
	if !errors.Is(err, ErrClientInventoryLimit) || !errors.Is(err, runtime.ErrCycleFailed) || !state.Complete || !state.Failed || reads.Load() != 0 {
		t.Fatal("inventory limit lost its failure", state, err)
	}
	state, err = runtime.ScanCycle(context.Background(), "realm-query", access, source, emit, nil, scan, budget)
	if err != nil || !state.Complete || state.Failed || reads.Load() != 1 {
		t.Fatal("later full scan did not restart", state, err)
	}
}
