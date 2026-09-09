package client

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"sync/atomic"
	"testing"
)

type completionTransport func(*http.Request) (*http.Response, error)

func (f completionTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type cancellationBody struct {
	cancel context.CancelFunc
	closed atomic.Bool
}

func (b *cancellationBody) Read(p []byte) (int, error) {
	b.cancel()
	return copy(p, "private-provider-body"), io.EOF
}
func (b *cancellationBody) Close() error { b.closed.Store(true); return nil }

func TestResponseCompletionRejectsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body := &cancellationBody{cancel: cancel}
	c := &Client{
		base:    &url.URL{Scheme: "https", Host: "provider.example"},
		permits: make(chan struct{}, MaxConcurrentRequests),
		http: &http.Client{Transport: completionTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: body}, nil
		})},
	}
	response, err := c.Do(ctx, "GET", "/record", nil, nil)
	if err == nil || response.StatusCode != 0 || len(response.Body) != 0 {
		t.Fatal("cancellation during response completion returned success or response data")
	}
	if !body.closed.Load() || len(c.permits) != 0 {
		t.Fatal("canceled completion retained the response body or request capacity")
	}
}
