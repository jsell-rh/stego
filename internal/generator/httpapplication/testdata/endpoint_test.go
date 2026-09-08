package sample

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"example.com/http-test/out/application/transport"
	"example.com/http-test/out/auth"
	"github.com/golang-jwt/jwt/v5"
)

func TestOrderingRejectsAmbiguousInput(t *testing.T) {
	fields := map[string]string{"name": "name", "created_at": "created_time", "created_time": "created_time"}
	order, err := transport.ParseOrderBy("name DESC, created_at", fields)
	if err != nil || len(order) != 2 || order[0].Direction != "desc" || order[1].Field != "created_time" || order[1].Direction != "asc" {
		t.Fatalf("ordering: %v %v", order, err)
	}
	for _, value := range []string{"unknown", "name desc; SELECT 1", "name desc extra", "name,", "name,name", "created_at,created_time", strings.Repeat("x", 513)} {
		if _, err := transport.ParseOrderBy(value, fields); err == nil {
			t.Fatalf("invalid order accepted: %s", value)
		}
	}
}

func handler(t *testing.T) (http.Handler, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := auth.NewVerifier(auth.Config{Issuer: "https://issuer.example", Audience: "records", PublicKey: &key.PublicKey})
	if err != nil {
		t.Fatal(err)
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.RegisteredClaims{Issuer: "https://issuer.example", Audience: jwt.ClaimStrings{"records"}, Subject: "reader", IssuedAt: jwt.NewNumericDate(time.Now().Add(-time.Minute)), ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}).SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := New(nil, verifier, nil)
	if err != nil {
		t.Fatal(err)
	}
	return handler, token
}
func TestEndpointInputAndIdentity(t *testing.T) {
	handler, token := handler(t)
	for _, test := range []struct {
		body   string
		status int
	}{
		{`{"title":"record"}`, 201},
		{`{"title":"record","details":{"label":"nested"}}`, 201},
		{`{"title":"\ud83d\ude00"}`, 201},
		{`{"title":"\\ud800"}`, 201},
		{`{"Title":"case alias"}`, 400},
		{`{"title":"record","details":{"Label":"case alias"}}`, 400},
		{`{"title":"record","details":{"label":"one","label":"two"}}`, 400},
		{`{"title":"one","t\u0069tle":"two"}`, 400},
		{`{"title":"one","unknown":true}`, 400},
		{`{"title":"\ud800"}`, 400},
		{`{"title":"\udc00"}`, 400},
		{`{"title":"\ud800\ud800"}`, 400},
		{"{\"title\":\"\xff\"}", 400},
		{`{"title":"record"} {}`, 400},
		{`[]`, 400},
		{`null`, 400},
		{`{"title":"` + strings.Repeat("x", transport.MaxRequestBytes) + `"}`, 400},
		{`{"title":"failure"}`, 500},
	} {
		before := Calls.Load()
		r := httptest.NewRequest("POST", "/records", strings.NewReader(test.body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, r)
		if response.Code != test.status {
			t.Fatalf("input %q: %d %s", test.body, response.Code, response.Body.String())
		}
		if test.status == 400 && Calls.Load() != before {
			t.Fatal("invalid request reached domain code")
		}
		if response.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("response permits caching")
		}
		if strings.Contains(response.Body.String(), "private database error") {
			t.Fatal("error details escaped")
		}
		if test.status == 201 && !strings.Contains(response.Body.String(), `"subject":"reader"`) {
			t.Fatal("verified identity did not reach domain code")
		}
	}
	for _, values := range [][]string{nil, {"Bearer invalid"}, {"Bearer " + token, "Bearer " + token}} {
		before := Calls.Load()
		r := httptest.NewRequest("POST", "/records", strings.NewReader("bad json"))
		r.Header["Authorization"] = values
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, r)
		if response.Code != 401 || Calls.Load() != before {
			t.Fatal("unauthenticated request reached decoding or domain code")
		}
	}
}
func TestEndpointStopsBodyReadOnCancellation(t *testing.T) {
	handler, token := handler(t)
	reader, writer := io.Pipe()
	defer writer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := httptest.NewRequest("POST", "/records", reader).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { handler.ServeHTTP(response, r); close(done) }()
	if _, err := writer.Write([]byte(`{"title":`)); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("body read did not stop")
	}
	if response.Code != 503 {
		t.Fatalf("canceled request: %d", response.Code)
	}
}

type signalBody struct {
	io.ReadCloser
	started chan struct{}
	once    sync.Once
}

func (b *signalBody) Read(data []byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	return b.ReadCloser.Read(data)
}

func TestEndpointCancelsARealSocketBodyRead(t *testing.T) {
	endpoint, token := handler(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = &signalBody{ReadCloser: r.Body, started: started}
		endpoint.ServeHTTP(w, r)
	}))
	server.Config.BaseContext = func(net.Listener) context.Context { return ctx }
	server.Start()
	defer server.Close()
	connection, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(connection, "POST /records HTTP/1.1\r\nHost: example.test\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: 256\r\n\r\n{\"title\":", token); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("request body read did not start")
	}
	cancel()
	response, err := http.ReadResponse(bufio.NewReader(connection), nil)
	if err != nil {
		t.Fatalf("canceled socket request: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != 503 {
		t.Fatalf("canceled socket request: %d", response.StatusCode)
	}
}

func TestNoContentEndpoint(t *testing.T) {
	authenticate := func(ctx context.Context, token string) (context.Context, error) {
		if token != "verified" {
			return nil, errors.New("denied")
		}
		return ctx, nil
	}
	decode := func(r *http.Request) (string, error) { return r.URL.Path, nil }
	writeError := func(w http.ResponseWriter, r *http.Request, err error) { w.WriteHeader(403) }
	for _, code := range []int{204, 205} {
		calls := 0
		endpoint, err := transport.Endpoint(authenticate, decode, func(ctx context.Context, path string) (transport.NoContent, error) {
			calls++
			if path == "/denied" {
				return transport.NoContent{}, errors.New("denied")
			}
			return transport.NoContent{}, nil
		}, code, writeError)
		if err != nil {
			t.Fatal(err)
		}
		server := httptest.NewServer(endpoint)
		for _, test := range []struct {
			path, token string
			status      int
		}{{"/record", "verified", code}, {"/denied", "verified", 403}, {"/record", "invalid", 403}} {
			request, _ := http.NewRequest("DELETE", server.URL+test.path, nil)
			request.Header.Set("Authorization", "Bearer "+test.token)
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || response.StatusCode != test.status || len(body) != 0 || response.Header.Get("Content-Type") != "" || response.Header.Get("Cache-Control") != "no-store" {
				t.Fatalf("empty response: %d %q %v %v", response.StatusCode, body, response.Header, err)
			}
		}
		server.Close()
		if calls != 2 {
			t.Fatalf("unauthenticated request reached domain: %d", calls)
		}
		if _, err := transport.Endpoint(authenticate, decode, func(context.Context, string) (string, error) { return "must not be discarded", nil }, code, writeError); err == nil {
			t.Fatal("body response accepted for an empty endpoint")
		}
	}
}

func TestDynamicReplyPreservesStatusAndBodyRules(t *testing.T) {
	for _, test := range []struct {
		status int
		body   bool
		want   int
	}{{202, true, 202}, {200, true, 200}, {204, false, 204}, {205, false, 205}, {204, true, 500}, {200, false, 500}, {400, true, 500}} {
		handler, err := transport.ReplyEndpoint(func(ctx context.Context, _ string) (context.Context, error) { return ctx, nil }, func(*http.Request) (struct{}, error) { return struct{}{}, nil }, func(context.Context, struct{}) (transport.Reply[Response], error) {
			reply := transport.Reply[Response]{Status: test.status}
			if test.body {
				reply.Value = &Response{Title: "pending"}
			}
			return reply, nil
		}, func(w http.ResponseWriter, _ *http.Request, _ error) { w.WriteHeader(500) })
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest("POST", "/records", nil)
		request.Header.Set("Authorization", "Bearer verified")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != test.want {
			t.Fatalf("dynamic status %d: got %d", test.status, recorder.Code)
		}
		if test.want == 204 || test.want == 205 {
			if recorder.Body.Len() != 0 || recorder.Header().Get("Content-Type") != "" {
				t.Fatal("empty reply has a body")
			}
		} else if test.want == 202 && !strings.Contains(recorder.Body.String(), "pending") {
			t.Fatal("pending reply lost its body")
		}
	}
}
