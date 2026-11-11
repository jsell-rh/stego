package auditsink

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	logs "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/proto"

	"github.com/jsell-rh/stego/registry/runtime/audit"
)

// authority is a test certificate authority that issues the certificates of the stand-in and of the client.
type authority struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newAuthority(t *testing.T, name string) *authority {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name}, IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &authority{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// issue gives a certificate of the authority for 127.0.0.1 (a server) or for the common name (a client), and its PEM
// certificate and key.
func (a *authority) issue(t *testing.T, name string, server bool) (tls.Certificate, []byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: name}, KeyUsage: x509.KeyUsageDigitalSignature,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	if server {
		template.ExtKeyUsage, template.IPAddresses = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, []net.IP{net.ParseIP("127.0.0.1")}
		template.DNSNames = []string{"audit-sink.test"}
	} else {
		template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, a.cert, &key.PublicKey, a.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return pair, certPEM, keyPEM
}

// writeFile writes data to a new file of the test's temporary folder and gives its path.
func writeFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// received is one request that the stand-in took.
type received struct {
	header  http.Header
	client  string
	request *collogs.ExportLogsServiceRequest
}

// standIn is a local OTLP/HTTP logs endpoint over TLS with its own certificate authority (ST-STD-LOCAL). It decodes
// each request and answers with the answer that the test sets: accept, a partial success that rejects records, or
// an HTTP status.
type standIn struct {
	server *httptest.Server
	ca     *authority
	mu     sync.Mutex
	got    []received
	answer func() (int, *collogs.ExportLogsServiceResponse)
}

// newStandIn starts the stand-in. It asks for a client certificate of clients and verifies one when it is given.
func newStandIn(t *testing.T, clients *authority) *standIn {
	return startStandIn(t, clients, tls.VerifyClientCertIfGiven)
}

// newStrictStandIn starts a stand-in that refuses a client with no client certificate of clients.
func newStrictStandIn(t *testing.T, clients *authority) *standIn {
	return startStandIn(t, clients, tls.RequireAndVerifyClientCert)
}

func startStandIn(t *testing.T, clients *authority, mode tls.ClientAuthType) *standIn {
	t.Helper()
	s := &standIn{ca: newAuthority(t, "stand-in CA")}
	s.answer = func() (int, *collogs.ExportLogsServiceResponse) {
		return http.StatusOK, &collogs.ExportLogsServiceResponse{}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/logs", func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		var req collogs.ExportLogsServiceRequest
		if err != nil || r.Header.Get("Content-Type") != "application/x-protobuf" || proto.Unmarshal(data, &req) != nil {
			http.Error(w, "not an OTLP logs request", http.StatusBadRequest)
			return
		}
		client := ""
		if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
			client = r.TLS.PeerCertificates[0].Subject.CommonName
		}
		s.mu.Lock()
		s.got = append(s.got, received{header: r.Header.Clone(), client: client, request: &req})
		answer := s.answer
		s.mu.Unlock()
		status, body := answer()
		out, _ := proto.Marshal(body)
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(status)
		_, _ = w.Write(out)
	})
	mux.HandleFunc("POST /moved", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/v1/logs", http.StatusPermanentRedirect)
	})
	s.server = httptest.NewUnstartedServer(mux)
	pair, _, _ := s.ca.issue(t, "127.0.0.1", true)
	s.server.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
	if clients != nil {
		pool := x509.NewCertPool()
		pool.AddCert(clients.cert)
		s.server.TLS.ClientAuth, s.server.TLS.ClientCAs = mode, pool
	}
	s.server.StartTLS()
	t.Cleanup(s.server.Close)
	return s
}

func (s *standIn) url(path string) string { return s.server.URL + path }

func (s *standIn) requests() []received {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]received(nil), s.got...)
}

func (s *standIn) setAnswer(answer func() (int, *collogs.ExportLogsServiceResponse)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.answer = answer
}

// mustSink makes the HTTPS sink of the settings and fails the test when it is refused.
func mustSink(t *testing.T, s Settings) *HTTPSSink {
	t.Helper()
	sink, err := NewHTTPSSink(s)
	if err != nil {
		t.Fatal(err)
	}
	return sink
}

// rejecting is the answer of a partial success that rejects n log records.
func rejecting(n int64) func() (int, *collogs.ExportLogsServiceResponse) {
	return func() (int, *collogs.ExportLogsServiceResponse) {
		return http.StatusOK, &collogs.ExportLogsServiceResponse{PartialSuccess: &collogs.ExportLogsPartialSuccess{
			RejectedLogRecords: n, ErrorMessage: "the record is refused"}}
	}
}

// attributes gives the text attributes of a log record by key.
func attributes(r *logs.LogRecord) map[string]string {
	m := map[string]string{}
	for _, kv := range r.GetAttributes() {
		m[kv.GetKey()] = kv.GetValue().GetStringValue()
	}
	return m
}

// records gives the log records of a request.
func records(req *collogs.ExportLogsServiceRequest) []*logs.LogRecord {
	var out []*logs.LogRecord
	for _, rl := range req.GetResourceLogs() {
		for _, sl := range rl.GetScopeLogs() {
			out = append(out, sl.GetLogRecords()...)
		}
	}
	return out
}

func TestHTTPSSink(t *testing.T) {
	// HTTPS_PROXY names a listener that counts its connections, set before the first send, because the proxy
	// settings of the environment are read once in a process. A loopback host is never sent through a proxy.
	proxy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	var proxied atomic.Int32
	go func() {
		for {
			conn, err := proxy.Accept()
			if err != nil {
				return
			}
			proxied.Add(1)
			_ = conn.Close()
		}
	}()
	t.Setenv("HTTPS_PROXY", "http://"+proxy.Addr().String())
	t.Setenv("https_proxy", "http://"+proxy.Addr().String())
	t.Setenv("NO_PROXY", "")
	e := newEnv(t)
	anchor := audit.Anchor{Tenant: "t1", Kind: audit.AnchorRetention, Head: bytes.Repeat([]byte{9}, 32), Count: 3}
	event := e.record(t, "t1", "r1", nil)
	clients := newAuthority(t, "client CA")
	_, clientCert, clientKey := clients.issue(t, "orders-worker", false)
	ctx := context.Background()

	t.Run("ST-SUP-RP05-LC16-T01 Each entry goes as one OTLP log record with its attributes and the token of the file", func(t *testing.T) {
		s := newStandIn(t, clients)
		token := writeFile(t, "token", []byte("token-one\n"))
		sink, err := NewHTTPSSink(Settings{URL: s.url("/v1/logs"), TokenFile: token, CAFile: writeFile(t, "ca.pem", s.ca.pem),
			ClientCertFile: writeFile(t, "client.pem", clientCert), ClientKeyFile: writeFile(t, "client.key", clientKey)})
		if err != nil {
			t.Fatal(err)
		}
		if err := sink.Append(ctx, []audit.Event{event}); err != nil {
			t.Fatalf("Append to the stand-in: %v", err)
		}
		// The token file is read again on each send: a rotated token goes on the next send with no restart.
		if err := os.WriteFile(token, []byte("token-two"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := sink.Anchor(ctx, anchor); err != nil {
			t.Fatalf("Anchor to the stand-in: %v", err)
		}
		got := s.requests()
		if len(got) != 2 {
			t.Fatalf("the stand-in took %d requests, want 2", len(got))
		}
		wantBody, err := audit.EventMessage(event)
		if err != nil {
			t.Fatal(err)
		}
		for i, want := range []struct {
			auth, id, kind, hash, prev string
			body                       []byte
		}{
			{"Bearer token-one", event.ID.String(), KindEvent, hex.EncodeToString(event.Hash), hex.EncodeToString(event.PrevHash), wantBody},
			{"Bearer token-two", "", KindAnchor, hex.EncodeToString(anchor.Head), "", audit.AnchorMessage(anchor)},
		} {
			r := got[i]
			if a := r.header.Get("Authorization"); a != want.auth || r.client != "orders-worker" {
				t.Errorf("request %d carried Authorization %q and the client certificate %q, want %q and orders-worker", i, a, r.client, want.auth)
			}
			recs := records(r.request)
			if len(recs) != 1 {
				t.Fatalf("request %d holds %d log records, want one", i, len(recs))
			}
			attrs := attributes(recs[0])
			if want.id == "" {
				want.id = "anchor-"
			}
			// The attribute keys are the wire names that a destination reads.
			if !strings.HasPrefix(attrs["stego.audit.entry_id"], want.id) || attrs["stego.audit.entry_kind"] != want.kind ||
				attrs["stego.audit.tenant"] != "t1" || attrs["stego.audit.hash"] != want.hash || attrs["stego.audit.prev_hash"] != want.prev {
				t.Errorf("request %d has the attributes %v, want the entry ID %s, kind %s, tenant t1, hash %s and previous hash %q", i, attrs, want.id, want.kind, want.hash, want.prev)
			}
			if body := recs[0].GetBody().GetStringValue(); body != string(want.body) {
				t.Errorf("request %d has the body\n%s\nwant the canonical entry\n%s", i, body, want.body)
			}
		}
	})

	t.Run("ST-SUP-RP05-LC16-T02 A named auth header carries the token of the file as it is", func(t *testing.T) {
		s := newStandIn(t, nil)
		sink, err := NewHTTPSSink(Settings{URL: s.url("/v1/logs"), TokenFile: writeFile(t, "token", []byte("ApiKey c2VjcmV0\n")),
			AuthHeader: "X-Api-Key", CAFile: writeFile(t, "ca.pem", s.ca.pem)})
		if err != nil {
			t.Fatal(err)
		}
		if err := sink.Anchor(ctx, anchor); err != nil {
			t.Fatal(err)
		}
		got := s.requests()
		if len(got) != 1 || got[0].header.Get("X-Api-Key") != "ApiKey c2VjcmV0" || got[0].header.Get("Authorization") != "" {
			t.Fatalf("the stand-in took %d requests with the headers %v, want X-Api-Key: ApiKey c2VjcmV0 and no Authorization", len(got), got)
		}
	})

	t.Run("ST-SUP-RP05-LC16-T03 A send counts only when the endpoint accepts every record over verified TLS", func(t *testing.T) {
		s := newStandIn(t, nil)
		token := writeFile(t, "token", []byte("token-one"))
		caFile := writeFile(t, "ca.pem", s.ca.pem)
		sink, err := NewHTTPSSink(Settings{URL: s.url("/v1/logs"), TokenFile: token, CAFile: caFile})
		if err != nil {
			t.Fatal(err)
		}
		for name, answer := range map[string]func() (int, *collogs.ExportLogsServiceResponse){
			"a partial success that rejects a record": rejecting(1),
			"503": func() (int, *collogs.ExportLogsServiceResponse) {
				return http.StatusServiceUnavailable, &collogs.ExportLogsServiceResponse{}
			},
			"400": func() (int, *collogs.ExportLogsServiceResponse) {
				return http.StatusBadRequest, &collogs.ExportLogsServiceResponse{}
			},
		} {
			s.setAnswer(answer)
			if err := sink.Anchor(ctx, anchor); !errors.Is(err, errNotAccepted) {
				t.Errorf("the answer %s gave %v, want an error of a send that is not accepted", name, err)
			}
		}
		s.setAnswer(rejecting(0))
		if err := sink.Anchor(ctx, anchor); err != nil {
			t.Errorf("a partial success that rejects no record gave %v, want an accepted send", err)
		}
		// The sink follows no redirect.
		before := len(s.requests())
		moved, err := NewHTTPSSink(Settings{URL: s.url("/moved"), TokenFile: token, CAFile: caFile})
		if err != nil {
			t.Fatal(err)
		}
		if err := moved.Anchor(ctx, anchor); !errors.Is(err, errNotAccepted) || len(s.requests()) != before {
			t.Errorf("a redirect gave %v after %d more requests, want a send that is not accepted and none", err, len(s.requests())-before)
		}
		// A CA file of another authority does not verify the stand-in.
		other := newAuthority(t, "another CA")
		wrong, err := NewHTTPSSink(Settings{URL: s.url("/v1/logs"), TokenFile: token, CAFile: writeFile(t, "other.pem", other.pem)})
		if err != nil {
			t.Fatal(err)
		}
		if err := wrong.Anchor(ctx, anchor); !errors.Is(err, errNotAccepted) || !strings.Contains(err.Error(), "certificate") || len(s.requests()) != before {
			t.Errorf("a send with another CA gave %v, want a refused certificate and no request", err)
		}
		// The sink uses no proxy: HTTPS_PROXY names the listener of the test, which counts its connections, and the
		// sink sends to a host name that is not loopback, which a dialer of the test resolves to the stand-in.
		_, port, _ := net.SplitHostPort(s.server.Listener.Addr().String())
		named, err := NewHTTPSSink(Settings{URL: "https://audit-sink.test:" + port + "/v1/logs", TokenFile: token, CAFile: caFile})
		if err != nil {
			t.Fatal(err)
		}
		dialer := &net.Dialer{}
		named.client.Transport.(*http.Transport).DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			if address != "audit-sink.test:"+port {
				return nil, errors.New("the test dials only the stand-in")
			}
			return dialer.DialContext(ctx, network, s.server.Listener.Addr().String())
		}
		before = len(s.requests())
		if err := named.Anchor(ctx, anchor); err != nil || proxied.Load() != 0 || len(s.requests()) != before+1 {
			t.Errorf("the send by host name gave %v with %d proxy connections and %d requests, want an accepted send, no proxy connection and one request",
				err, proxied.Load(), len(s.requests())-before)
		}
		// A failed send to a URL with a secret in its query gives an error without the query.
		closed := mustSink(t, Settings{URL: "https://127.0.0.1:1/v1/logs?key=s3cret-query", TokenFile: token, CAFile: caFile})
		if err := closed.Anchor(ctx, anchor); err == nil || strings.Contains(err.Error(), "s3cret") || strings.Contains(err.Error(), "key=") {
			t.Errorf("a failed send to a URL with a query gave %v, want an error without the query", err)
		}
		// A client certificate of another authority is refused by the endpoint.
		_, badCert, badKey := other.issue(t, "intruder", false)
		strict := newStrictStandIn(t, clients)
		intruder, err := NewHTTPSSink(Settings{URL: strict.url("/v1/logs"), CAFile: writeFile(t, "strict.pem", strict.ca.pem),
			ClientCertFile: writeFile(t, "bad.pem", badCert), ClientKeyFile: writeFile(t, "bad.key", badKey)})
		if err != nil {
			t.Fatal(err)
		}
		if err := intruder.Anchor(ctx, anchor); !errors.Is(err, errNotAccepted) || len(strict.requests()) != 0 {
			t.Errorf("a client certificate of another authority gave %v, want a refused send", err)
		}
	})

	t.Run("ST-SUP-RP05-LC16-T04 Nothing leaves the outgoing queue until the endpoint accepts the entry", func(t *testing.T) {
		s := newStandIn(t, nil)
		s.setAnswer(rejecting(1))
		sink, err := NewHTTPSSink(Settings{URL: s.url("/v1/logs"), TokenFile: writeFile(t, "token", []byte("token-one")),
			CAFile: writeFile(t, "ca.pem", s.ca.pem)})
		if err != nil {
			t.Fatal(err)
		}
		queued := e.record(t, "t2", "r2", nil)
		pending := "SELECT count(*) FROM stego_outbox.messages WHERE id = $1::uuid AND state <> 'done'"
		stop := e.startWorker(t, sink)
		retried := waitFor(func() bool { return len(s.requests()) >= 3 })
		if !retried || e.count(t, pending, queued.ID.String()) != 1 {
			stop()
			t.Fatalf("the rejected entry was not sent again and kept in the outbox (%d requests)", len(s.requests()))
		}
		s.setAnswer(rejecting(0))
		landed := waitFor(func() bool { return e.count(t, pending, queued.ID.String()) == 0 })
		stop()
		if !landed {
			t.Fatalf("the entry did not leave the outbox once the endpoint accepted it")
		}
		// The entry was sent while the endpoint rejected it and again when it accepted it.
		sends := 0
		for _, r := range s.requests() {
			for _, rec := range records(r.request) {
				if attributes(rec)[AttributeEntryID] == queued.ID.String() {
					sends++
				}
			}
		}
		if sends < 2 {
			t.Fatalf("the queued entry was sent %d times, want a rejected send and an accepted send", sends)
		}
	})
}
