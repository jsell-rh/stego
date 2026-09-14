package client

import (
	"context"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func tlsOptions(t *testing.T, handler http.HandlerFunc) Options {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	return Options{BaseURL: server.URL, CAFile: ca}
}

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("HTTP operation did not finish within the test limit")
		var zero T
		return zero
	}
}

// This test uses the public API that exists before separate stream limits.
// It must fail if either class can use the other class's request slots.
func TestStreamRequestIsolation(t *testing.T) {
	for _, streamsFirst := range []bool{true, false} {
		name := "requests-first"
		if streamsFirst {
			name = "streams-first"
		}
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			requests := make(chan struct{}, 64)
			frames := make(chan struct{}, 64)
			options := tlsOptions(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path == "/watch" {
					_, _ = w.Write([]byte("event\n"))
					w.(http.Flusher).Flush()
				} else {
					requests <- struct{}{}
				}
				<-r.Context().Done()
			})
			client, err := New(options)
			if err != nil {
				t.Fatal(err)
			}
			var running sync.WaitGroup
			t.Cleanup(func() { client.Close(); running.Wait() })
			type call struct {
				cancel context.CancelFunc
				done   chan error
			}
			start := func(stream bool) call {
				ctx, cancel := context.WithCancel(context.Background())
				result := call{cancel: cancel, done: make(chan error, 1)}
				running.Go(func() {
					var err error
					if stream {
						_, err = client.Stream(ctx, "/watch", nil, func(context.Context, []byte) error { frames <- struct{}{}; return nil })
					} else {
						_, err = client.Do(ctx, "GET", "/ownership", nil, nil)
					}
					result.done <- err
				})
				entered := requests
				if stream {
					entered = frames
				}
				select {
				case <-entered:
				case err := <-result.done:
					cancel()
					t.Fatalf("one request class blocked the other: stream=%v: %v", stream, err)
				case <-time.After(5 * time.Second):
					cancel()
					t.Fatal("shared connection limit blocked an admitted request")
				}
				return result
			}
			var streams, ordinary []call
			for _, stream := range []bool{streamsFirst, !streamsFirst} {
				// The existing default request limit and the new default stream
				// limit are both 16. Keep the baseline test independent of new APIs.
				for range 16 {
					result := start(stream)
					if stream {
						streams = append(streams, result)
					} else {
						ordinary = append(ordinary, result)
					}
				}
			}
			before := calls.Load()
			if _, err := client.Stream(context.Background(), "/watch", nil, func(context.Context, []byte) error { return nil }); err == nil {
				t.Fatal("excess stream was accepted")
			}
			if _, err := client.Do(context.Background(), "GET", "/ownership", nil, nil); err == nil {
				t.Fatal("excess ordinary request was accepted")
			}
			if calls.Load() != before {
				t.Fatal("excess request reached the server")
			}
			// Cancellation must restore only the slot held by that call.
			for _, group := range [][]call{streams, ordinary} {
				group[0].cancel()
				if receive(t, group[0].done) == nil {
					t.Fatal("canceled call returned success")
				}
			}
			streams[0] = start(true)
			ordinary[0] = start(false)
			client.Close()
			for _, group := range [][]call{streams, ordinary} {
				for _, call := range group {
					if receive(t, call.done) == nil {
						t.Error("close did not cancel an active call")
					}
					call.cancel()
				}
			}
			before = calls.Load()
			if _, err := client.Stream(context.Background(), "/watch", nil, func(context.Context, []byte) error { return nil }); err == nil {
				t.Fatal("closed client accepted a stream")
			}
			if _, err := client.Do(context.Background(), "GET", "/ownership", nil, nil); err == nil {
				t.Fatal("closed client accepted a request")
			}
			if calls.Load() != before {
				t.Fatal("closed client contacted the server")
			}
		})
	}
}

func TestStreamCallbackOwnsItsSlot(t *testing.T) {
	options := tlsOptions(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("event\n")) })
	client, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{}, 16)
	done := make(chan error, 16)
	var running sync.WaitGroup
	defer func() { cancel(); running.Wait() }()
	want := errors.New("consumer stopped")
	for range 16 {
		running.Go(func() {
			_, err := client.Stream(ctx, "/watch", nil, func(ctx context.Context, _ []byte) error {
				entered <- struct{}{}
				<-ctx.Done()
				return want
			})
			done <- err
		})
	}
	for range 16 {
		receive(t, entered)
	}
	if _, err := client.Stream(ctx, "/watch", nil, func(context.Context, []byte) error { return nil }); err == nil {
		t.Fatal("callback released its slot before completion")
	}
	if response, err := client.Do(ctx, "GET", "/ownership", nil, nil); err != nil || response.StatusCode != 200 {
		t.Fatal("callbacks blocked an ordinary request", err)
	}
	cancel()
	for range 16 {
		if !errors.Is(receive(t, done), want) {
			t.Fatal("callback error changed")
		}
	}
	if response, err := client.Stream(context.Background(), "/watch", nil, func(context.Context, []byte) error { return nil }); err != nil || response.StatusCode != 200 {
		t.Fatal("completed callbacks retained stream slots", err)
	}
}
