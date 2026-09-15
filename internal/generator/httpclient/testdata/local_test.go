package client

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestLocalApplicationContracts(t *testing.T) {
	c := NewLocalApplication()
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	response, err := c.Do(ctx, "GET", "/api/v1/auth/whoami", http.Header{"Authorization": {"Bearer server-session-token"}}, nil)
	if err != nil || response.StatusCode != 200 || string(response.Body) != `{"user":"owner"}` {
		t.Fatal("identity contract changed", err, response.StatusCode)
	}
	response, err = c.Do(ctx, "POST", "/api/v1/workspaces", http.Header{"Content-Type": {"application/json"}}, []byte(`{"name":"workspace"}`))
	if err != nil || response.StatusCode != 201 || string(response.Body) != `{"name":"workspace"}` {
		t.Fatal("write contract changed", err, response.StatusCode)
	}
	var frames []string
	_, err = c.Stream(ctx, "/stream", nil, func(_ context.Context, frame []byte) error { frames = append(frames, string(frame)); return nil })
	if err != nil || strings.Join(frames, ",") != "first,second" {
		t.Fatal("stream contract changed", err)
	}
}

func TestLocalApplicationCannotChangeDestination(t *testing.T) {
	c := NewLocalApplication()
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, path := range []string{"http://example.com/", "https://example.com/", "//example.com/", "/../escape", "/%2e%2e/escape", "/%2f%2fexample.com/", "/redirect"} {
		if _, err := c.Do(ctx, "GET", path, nil, nil); err == nil {
			t.Fatalf("accepted destination %q", path)
		}
	}
	for _, address := range []string{"localhost:8080", "127.0.0.2:8080", "[::1]:8080", "192.0.2.1:8080"} {
		if conn, err := c.transport.DialContext(ctx, "tcp", address); err == nil {
			conn.Close()
			t.Fatalf("accepted dial address %q", address)
		}
	}
	if conn, err := c.transport.DialContext(ctx, "udp", c.base.Host); err == nil {
		conn.Close()
		t.Fatal("accepted UDP")
	}
	if _, err := New(Options{BaseURL: c.base.String()}); err == nil {
		t.Fatal("ordinary constructor accepted plaintext")
	}
	if _, err := c.Do(ctx, "GET", "/", http.Header{"Host": {"example.com"}}, nil); err == nil {
		t.Fatal("accepted host override")
	}
}

func TestLocalApplicationCloseCancelsRequest(t *testing.T) {
	c := NewLocalApplication()
	defer c.Close()
	probe := NewLocalApplication()
	defer probe.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := c.Do(ctx, "GET", "/wait", nil, nil); result <- err }()
	for {
		response, err := probe.Do(ctx, "GET", "/waiting", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if string(response.Body) == "1" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("request did not start")
		case <-time.After(5 * time.Millisecond):
		}
	}
	c.Close()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("closed request succeeded")
		}
	case <-ctx.Done():
		t.Fatal("close did not cancel the request")
	}
	if _, err := c.Do(ctx, "GET", "/", nil, nil); err == nil {
		t.Fatal("closed client accepted work")
	}
}
