package browser

import (
	"bufio"
	"context"
	"errors"
	client "example.com/browser-test/out/browser/client"
	tracing "example.com/browser-test/out/tracing"
	"github.com/coder/websocket"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A wrapper can retain HTTP deadlines at the instant of a hijack.
// The generated socket writer must clear them after the connection transfers.
type tightDeadlineWriter struct{ http.ResponseWriter }

func (w tightDeadlineWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w tightDeadlineWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err != nil {
		return nil, nil, err
	}
	if err := conn.SetDeadline(time.Now().Add(80 * time.Millisecond)); err != nil {
		conn.Close()
		return nil, nil, err
	}
	return conn, rw, nil
}

func socketServer(t *testing.T, f *fixture) (*Backend, *httptest.Server, <-chan int) {
	t.Helper()
	b := f.start(t)
	complete := make(chan int, 8)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		result := tracing.ObserveHTTP(b, tightDeadlineWriter{w}, r)
		complete <- result
	}))
	server.Config.ReadTimeout = 80 * time.Millisecond
	server.Config.WriteTimeout = 80 * time.Millisecond
	server.StartTLS()
	b.origin, _ = url.Parse(server.URL)
	t.Cleanup(server.Close)
	return b, server, complete
}
func openSocket(t *testing.T, server *httptest.Server, c *http.Cookie) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, server.URL+apiPrefix+"/socket", &websocket.DialOptions{HTTPClient: server.Client(), HTTPHeader: http.Header{"Origin": {server.URL}, "Cookie": {c.String()}, "X-Forwarded-User": {"attacker"}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	return conn
}
func TestApplicationSocketDelivery(t *testing.T) {
	f := applicationFixture(t)
	c := applicationLogin(t, f)
	_, server, complete := socketServer(t, f)
	conn := openSocket(t, server, c)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// The connection must survive the ordinary HTTP server deadline.
	time.Sleep(120 * time.Millisecond)
	for _, kind := range []websocket.MessageType{websocket.MessageBinary, websocket.MessageText} {
		data := []byte("stdin")
		if kind == websocket.MessageText {
			data = []byte(`{"type":"resize","cols":80,"rows":24}`)
		}
		if err := conn.Write(ctx, kind, data); err != nil {
			t.Fatal(err)
		}
		got, payload, err := conn.Read(ctx)
		if err != nil || got != kind || string(payload) != string(data) {
			t.Fatal("socket delivery changed", err)
		}
	}
	if err := conn.Write(ctx, websocket.MessageBinary, []byte("exit")); err != nil {
		t.Fatal(err)
	}
	_, _, err := conn.Read(ctx)
	var closed websocket.CloseError
	require(t, errors.As(err, &closed) && closed.Code == websocket.StatusNormalClosure && closed.Reason == "17", "terminal exit status changed")
	select {
	case code := <-complete:
		require(t, code == 101, "telemetry wrapper lost the upgrade status")
	case <-ctx.Done():
		t.Fatal("socket handler did not finish")
	}
}
func TestApplicationSocketDenied(t *testing.T) {
	f := applicationFixture(t)
	c := applicationLogin(t, f)
	_, server, _ := socketServer(t, f)
	for _, tc := range []struct {
		headers http.Header
		query   string
		status  int
	}{
		{http.Header{"Origin": {server.URL}}, "", 401},
		{http.Header{"Origin": {"https://wrong.example"}, "Cookie": {c.String()}}, "", 403},
		{http.Header{"Cookie": {c.String()}}, "", 403},
		{http.Header{"Origin": {server.URL}, "Cookie": {c.String()}, "Authorization": {"Bearer browser-token"}}, "", 400},
		{http.Header{"Origin": {server.URL}, "Cookie": {c.String()}}, "?denied=1", 403},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		conn, response, err := websocket.Dial(ctx, server.URL+apiPrefix+"/socket"+tc.query, &websocket.DialOptions{HTTPClient: server.Client(), HTTPHeader: tc.headers})
		cancel()
		if conn != nil {
			conn.CloseNow()
		}
		require(t, err != nil && response != nil && response.StatusCode == tc.status, "socket denial changed")
	}
}
func TestApplicationSocketSessionEnd(t *testing.T) {
	if os.Getenv("STEGO_TEST_POSTGRES_DSN") == "" && os.Getenv("STEGO_REQUIRE_POSTGRES") != "1" {
		t.Skip("database fixture is not configured")
	}
	for _, mode := range []string{"local logout", "remote logout", "backend close", "runtime stop", "store failure", "session expiry", "token expiry"} {
		t.Run(mode, func(t *testing.T) {
			f := applicationFixture(t)
			c, csrf := login(t, f)
			b, server, complete := socketServer(t, f)
			if mode == "session expiry" || mode == "token expiry" {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				value, _, _, err := b.store.read(ctx, c.Value)
				if err != nil {
					cancel()
					t.Fatal(err)
				}
				if mode == "session expiry" {
					value.Expires = time.Now().Add(3 * time.Second).Unix()
				} else {
					value.AccessExpires = time.Now().Add(20 * time.Second).Unix()
				}
				payload, err := b.store.seal(c.Value, value)
				if err != nil {
					cancel()
					t.Fatal(err)
				}
				hash, err := sessionHash(c.Value)
				if err != nil {
					cancel()
					t.Fatal(err)
				}
				_, err = f.db.ExecContext(ctx, "UPDATE public.stego_browser_sessions SET payload=$1,expires_at=$2 WHERE id_hash=$3", payload, time.Unix(value.Expires, 0), hash)
				cancel()
				if err != nil {
					t.Fatal(err)
				}
			}
			conn := openSocket(t, server, c)
			switch mode {
			case "local logout":
				r := httptest.NewRequest("POST", server.URL+"/auth/logout", nil)
				r.Header.Set("Origin", server.URL)
				r.Header.Set(CSRFHeader, csrf)
				r.AddCookie(c)
				w := httptest.NewRecorder()
				b.ServeHTTP(w, r)
				require(t, w.Code == 204, "local logout failed")
			case "remote logout":
				require(t, send(f.backend, "POST", "/auth/logout", "", []*http.Cookie{c}, http.Header{"Origin": {origin}, CSRFHeader: {csrf}}).Code == 204, "remote logout failed")
			case "store failure":
				require(t, f.db.Close() == nil, "database close failed")
			case "backend close":
				b.Close()
			case "runtime stop":
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				require(t, b.Run(ctx) == nil, "runtime stop failed")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 23*time.Second)
			defer cancel()
			_, _, err := conn.Read(ctx)
			require(t, err != nil && ctx.Err() == nil, "session end did not close the socket")
			select {
			case <-complete:
			case <-ctx.Done():
				t.Fatal("socket work did not stop")
			}
			b.sockets.mu.Lock()
			count := len(b.sockets.leases)
			b.sockets.mu.Unlock()
			require(t, count == 0, "socket lease remained after close")
		})
	}
}
func TestApplicationSocketAdmission(t *testing.T) {
	var set socketSet
	id, _ := randomValue()
	ctx, release, err := set.acquire(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	_, second, err := set.acquire(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	defer second()
	if _, _, err := set.acquire(context.Background(), id); !errors.Is(err, errBusy) {
		t.Fatal("session socket limit failed")
	}
	set.end(id)
	require(t, ctx.Err() != nil, "session cancellation failed")
	release()
	second()
	var releases []func()
	for i := 0; i < socketCapacity; i++ {
		id, _ := randomValue()
		_, release, err := set.acquire(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	other, _ := randomValue()
	if _, _, err := set.acquire(context.Background(), other); !errors.Is(err, errBusy) {
		t.Fatal("global socket limit failed")
	}
	set.close()
	for _, release := range releases {
		release()
	}
	if _, _, err := set.acquire(context.Background(), id); !errors.Is(err, errBusy) {
		t.Fatal("closed backend accepted a socket")
	}
}
func TestSocketTransportIsolationAndClose(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ordinary" {
			w.WriteHeader(204)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _, _ = conn.Read(ctx)
	}))
	defer server.Close()
	c, err := client.New(client.Options{BaseURL: server.URL, CAFile: trust(t, server), StreamLimit: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	entered := make(chan struct{})
	ended := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	go func() {
		_, err := c.Socket(ctx, "/socket", "server-token", func(ctx context.Context, conn *websocket.Conn) error { close(entered); <-ctx.Done(); return ctx.Err() })
		ended <- err
	}()
	select {
	case <-entered:
	case err := <-ended:
		t.Fatal("socket handshake failed", err)
	case <-ctx.Done():
		t.Fatal("socket did not start")
	}
	response, err := c.Do(ctx, "GET", "/ordinary", nil, nil)
	require(t, err == nil && response.StatusCode == 204, "socket blocked ordinary HTTP")
	_, err = c.Socket(ctx, "/socket", "server-token", func(context.Context, *websocket.Conn) error { return nil })
	require(t, errors.Is(err, client.ErrSocketCapacity), "socket stream limit failed")
	c.Close()
	select {
	case err := <-ended:
		require(t, err != nil, "closed socket succeeded")
	case <-ctx.Done():
		t.Fatal("client close did not stop socket work")
	}
}
func TestSocketTransportRejectsRedirectsAndInvalidRequests(t *testing.T) {
	var reached atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/target" {
			reached.Store(true)
		}
		http.Redirect(w, r, "/target", 302)
	}))
	defer server.Close()
	c, err := client.New(client.Options{BaseURL: server.URL, CAFile: trust(t, server)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	consume := func(context.Context, *websocket.Conn) error { t.Error("invalid request invoked callback"); return nil }
	for _, path := range []string{"https://other.example/", "//other.example/", "/../escape", "/%2fescape", "/redirect"} {
		if _, err := c.Socket(ctx, path, "token", consume); err == nil {
			t.Fatal("invalid socket request accepted", path)
		}
	}
	for _, token := range []string{"", "value\r\nCookie: bad", strings.Repeat("x", 16385)} {
		if _, err := c.Socket(ctx, "/socket", token, consume); err == nil {
			t.Fatal("invalid socket token accepted")
		}
	}
	require(t, !reached.Load(), "WebSocket transport followed a redirect")
}
func TestSocketControlTrafficLimit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ping, pong := client.SocketControlHandlers(cancel)
	for i := 0; i < 512; i++ {
		require(t, ping(ctx, nil), "control budget ended early")
		pong(ctx, nil)
	}
	require(t, !ping(ctx, nil) && ctx.Err() != nil, "control budget did not cancel the stream")
}

func TestSocketDataBudget(t *testing.T) {
	for _, budget := range []socketBudget{{messages: client.SocketMessages}, {bytes: client.SocketBytes}} {
		require(t, !budget.accept(1), "data budget accepted excess data")
	}
	budget := socketBudget{messages: client.SocketMessages - 1, bytes: client.SocketBytes - 1}
	require(t, budget.accept(1) && !budget.accept(0), "data budget did not count an empty message")
	require(t, !new(socketBudget).accept(client.SocketMessageBytes+1), "oversized message passed the data budget")
}
func TestSocketTransportMessageLimit(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = conn.Write(ctx, websocket.MessageBinary, make([]byte, client.SocketMessageBytes+1))
	}))
	defer server.Close()
	c, err := client.New(client.Options{BaseURL: server.URL, CAFile: trust(t, server)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	rejected := false
	_, err = c.Socket(ctx, "/socket", "token", func(ctx context.Context, conn *websocket.Conn) error {
		_, data, err := conn.Read(ctx)
		rejected = errors.Is(err, websocket.ErrMessageTooBig) && len(data) <= client.SocketMessageBytes+1
		return err
	})
	require(t, err != nil && rejected, "oversized socket message was accepted")
}

func TestSocketTransportCloseCancelsRead(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		_, _, _ = conn.Read(ctx)
	}))
	defer server.Close()
	c, err := client.New(client.Options{BaseURL: server.URL, CAFile: trust(t, server)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	entered := make(chan struct{})
	ended := make(chan error, 1)
	go func() {
		_, err := c.Socket(ctx, "/socket", "token", func(ctx context.Context, conn *websocket.Conn) error {
			close(entered)
			_, _, err := conn.Read(ctx)
			return err
		})
		ended <- err
	}()
	select {
	case <-entered:
	case err := <-ended:
		t.Fatal("socket did not start", err)
	case <-ctx.Done():
		t.Fatal("socket did not start")
	}
	c.Close()
	select {
	case err := <-ended:
		require(t, err != nil, "closed read succeeded")
	case <-ctx.Done():
		t.Fatal("client close did not stop the read")
	}
}
