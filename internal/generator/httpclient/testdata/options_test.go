package client

import (
	"context"
	"net/http"
	"testing"
)

func TestStreamLimitOptions(t *testing.T) {
	options := tlsOptions(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("event\n")) })
	for _, limit := range []int{-1, MaxStreamLimit + 1} {
		options.StreamLimit = limit
		if c, err := New(options); err == nil {
			c.Close()
			t.Fatalf("invalid stream limit accepted: %d", limit)
		}
	}
	for _, limit := range []int{0, 1, 4, MaxStreamLimit} {
		options.StreamLimit = limit
		c, err := New(options)
		if err != nil {
			t.Fatal(err)
		}
		want := limit
		if want == 0 {
			want = DefaultStreamLimit
		}
		if c.StreamLimit() != want {
			t.Fatal("configured stream limit changed")
		}
		if _, err := c.Stream(context.Background(), "/watch", nil, func(context.Context, []byte) error { return nil }); err != nil {
			t.Fatal(err)
		}
		c.Close()
	}
	var missing *Client
	if missing.StreamLimit() != 0 {
		t.Fatal("nil client has stream capacity")
	}
}

func TestConfiguredStreamLimitEnforced(t *testing.T) {
	options := tlsOptions(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/watch" {
			_, _ = w.Write([]byte("event\n"))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}
	})
	options.StreamLimit = 1
	c, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, done := make(chan struct{}, 1), make(chan error, 1)
	go func() {
		_, err := c.Stream(ctx, "/watch", nil, func(context.Context, []byte) error { entered <- struct{}{}; return nil })
		done <- err
	}()
	defer func() { cancel(); receive(t, done) }()
	receive(t, entered)
	if _, err := c.Stream(ctx, "/watch", nil, func(context.Context, []byte) error { return nil }); err == nil {
		t.Fatal("configured stream limit was exceeded")
	}
	if response, err := c.Do(ctx, "GET", "/ownership", nil, nil); err != nil || response.StatusCode != 200 {
		t.Fatal("configured stream limit blocked ordinary requests", err)
	}
}
