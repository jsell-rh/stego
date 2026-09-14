package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"
)

func TestNamespaceReadsWithOpenWatches(t *testing.T) {
	watching := make(chan string, 16)
	client, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer one" {
			t.Error("watch or namespace read has no credential")
		}
		if r.URL.Path == "/api/v1/namespaces/owned" {
			_ = json.NewEncoder(w).Encode(Object{"metadata": Object{"name": "owned", "uid": "owned-uid", "resourceVersion": "21"}})
			return
		}
		if r.URL.Query().Get("watch") == "true" {
			_ = json.NewEncoder(w).Encode(Object{"type": "BOOKMARK", "object": Object{"metadata": Object{"resourceVersion": "20"}}})
			w.(http.Flusher).Flush()
			watching <- r.URL.Path
			<-r.Context().Done()
			return
		}
		_ = json.NewEncoder(w).Encode(Object{"metadata": Object{"resourceVersion": "20"}, "items": []Object{}})
	})
	ctx, cancel := context.WithCancel(context.Background())
	var running sync.WaitGroup
	done := make(chan error, 16)
	defer func() { cancel(); running.Wait() }()
	for i := range 16 {
		path := fmt.Sprintf("/api/v1/namespaces/owned-%d/pods", i)
		running.Go(func() {
			done <- client.Observe(ctx, Collection{Path: path}, func(Change) error { return nil })
		})
		select {
		case got := <-watching:
			if got != path {
				t.Fatalf("wrong namespace watch: %s", got)
			}
		case err := <-done:
			t.Fatal("namespace watch stopped", err)
		case <-time.After(5 * time.Second):
			t.Fatal("namespace watch did not start")
		}
	}
	object, status, err := client.Request(ctx, "GET", "/api/v1/namespaces/owned", nil)
	if err != nil || status != 200 || String(object, "metadata", "uid") != "owned-uid" {
		t.Fatalf("open namespace watches blocked ownership read: status=%d: %v", status, err)
	}
	cancel()
	for range 16 {
		select {
		case err := <-done:
			if err == nil {
				t.Error("canceled namespace watch returned success")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("namespace watch did not stop")
		}
	}
}
