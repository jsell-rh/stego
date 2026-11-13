package kafkaoutbox

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"
	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/noop"

	"github.com/jsell-rh/stego/registry/runtime/outbox"
)

// The tests of this file run against kfake, franz-go's in-process Kafka broker, and against hand-made listeners, so
// they can make a broker misbehave in ways a real broker does not.

const fakeTopic = "events"

// recorder is a log provider that keeps the failure class of each event kafka.delivery_failed.
type recorder struct {
	noop.LoggerProvider
	mu      sync.Mutex
	classes []string
}

func (r *recorder) Logger(string, ...otellog.LoggerOption) otellog.Logger { return recordLogger{r: r} }

func (r *recorder) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.classes...)
}

type recordLogger struct {
	noop.Logger
	r *recorder
}

func (l recordLogger) Emit(_ context.Context, record otellog.Record) {
	if record.EventName() != "kafka.delivery_failed" {
		return
	}
	record.WalkAttributes(func(kv attribute.KeyValue) bool {
		if kv.Key == "failure" {
			l.r.mu.Lock()
			l.r.classes = append(l.r.classes, kv.Value.AsString())
			l.r.mu.Unlock()
		}
		return true
	})
}

// eventNames is a log provider that keeps the event name and the severity of each record.
type eventNames struct {
	noop.LoggerProvider
	mu    sync.Mutex
	names []string
}

func (r *eventNames) Logger(string, ...otellog.LoggerOption) otellog.Logger { return eventLogger{r: r} }

func (r *eventNames) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.names...)
}

type eventLogger struct {
	noop.Logger
	r *eventNames
}

func (l eventLogger) Emit(_ context.Context, record otellog.Record) {
	l.r.mu.Lock()
	l.r.names = append(l.r.names, record.EventName()+" "+record.SeverityText())
	l.r.mu.Unlock()
}

// deliver delivers one event of tenant t1 through d within limit, and gives the error and the time it took.
func deliver(t *testing.T, d *Destination, limit time.Duration) (error, time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	id := uuid.New()
	start := time.Now()
	err := d.Deliver(ctx, nil, outbox.Delivery{Tenant: mustTenant(t, "t1"), Message: outbox.Message{ID: id,
		Destination: DestinationName, ResourceKey: "order:o-1", Kind: "order.changed", Payload: json.RawMessage(`{}`),
		OutgoingKey: id.String()}})
	return err, time.Since(start)
}

// fake starts a kfake broker with the topic events, listening where listen says, and closes it when the test ends.
func fake(t *testing.T, listen func(network, address string) (net.Listener, error), options ...kfake.Opt) *kfake.Cluster {
	t.Helper()
	options = append([]kfake.Opt{kfake.NumBrokers(1), kfake.SeedTopics(1, fakeTopic), kfake.ListenFn(listen)}, options...)
	c, err := kfake.NewCluster(options...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

// on listens on host at a free port.
func on(host string) func(string, string) (net.Listener, error) {
	return func(network, _ string) (net.Listener, error) { return net.Listen(network, net.JoinHostPort(host, "0")) }
}

// forward listens on 127.0.0.1 and forwards each connection to target, until the test ends.
func forward(t *testing.T, target string) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			client, err := l.Accept()
			if err != nil {
				return
			}
			server, err := net.Dial("tcp", target)
			if err != nil {
				_ = client.Close()
				continue
			}
			go func() { _, _ = io.Copy(server, client); _ = server.Close() }()
			go func() { _, _ = io.Copy(client, server); _ = client.Close() }()
		}
	}()
	return l.Addr().String()
}

// stored counts the records of the topic events of the broker at address.
func stored(t *testing.T, address string) int {
	t.Helper()
	client, err := kgo.NewClient(kgo.SeedBrokers(address), kgo.ConsumeTopics(fakeTopic),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()), kgo.FetchMaxWait(100*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	n := 0
	end := time.Now().Add(time.Second)
	for time.Now().Before(end) {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		n += len(client.PollFetches(ctx).Records())
		cancel()
	}
	return n
}

// otherAddress gives an IPv4 address of this host that is not a loopback address.
func otherAddress(t *testing.T) string {
	t.Helper()
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range addresses {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() && !n.IP.IsLinkLocalUnicast() {
			return n.IP.String()
		}
	}
	t.Fatal("this host has no IPv4 address that is not a loopback address")
	return ""
}

// certificates makes a CA and a server certificate that it signs for dnsName or ip, writes the CA to a PEM file, and
// gives the server's TLS configuration and the path of the CA file.
func certificates(t *testing.T, dnsName string, ip net.IP) (*tls.Config, string) {
	t.Helper()
	make := func(name string, parent *x509.Certificate, parentKey *ecdsa.PrivateKey, ca bool) (*x509.Certificate, *ecdsa.PrivateKey) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		template := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
		if ca {
			template.IsCA, template.BasicConstraintsValid, template.KeyUsage = true, true, x509.KeyUsageCertSign
		} else {
			template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
			if dnsName != "" {
				template.DNSNames = []string{dnsName}
			}
			if ip != nil {
				template.IPAddresses = []net.IP{ip}
			}
		}
		if parent == nil {
			parent, parentKey = template, key
		}
		der, err := x509.CreateCertificate(rand.Reader, template, parent, &key.PublicKey, parentKey)
		if err != nil {
			t.Fatal(err)
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		return cert, key
	}
	ca, caKey := make("test CA", nil, nil, true)
	server, serverKey := make("broker", ca, caKey, false)
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{server.Raw},
		PrivateKey: serverKey}}}, path
}

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBrokerSafety(t *testing.T) {
	t.Run("ST-SUP-RP01-LC32-T05 In the insecure loopback mode a broker named at another address gets no connection", func(t *testing.T) {
		c := fake(t, on(otherAddress(t)))
		seed := forward(t, c.ListenAddrs()[0])
		logs := &recorder{}
		d, err := New(Settings{Brokers: []string{seed}, Topic: fakeTopic, InsecureLoopback: true}, logs)
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		if err, _ := deliver(t, d, 3*time.Second); err == nil {
			t.Fatal("the destination sent plaintext to a broker that is not at a loopback address")
		}
		if n := stored(t, c.ListenAddrs()[0]); n != 0 {
			t.Fatalf("the broker at %s holds %d records", c.ListenAddrs()[0], n)
		}
	})

	t.Run("ST-SUP-RP01-LC32-T06 With a broker host list, a broker named at another host gets no connection", func(t *testing.T) {
		c := fake(t, on("127.0.0.2"))
		seed := forward(t, c.ListenAddrs()[0])
		d, err := New(Settings{Brokers: []string{seed}, BrokerHosts: []string{"127.0.0.1"}, Topic: fakeTopic, InsecureLoopback: true}, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		if err, _ := deliver(t, d, 3*time.Second); err == nil {
			t.Fatal("the destination dialed 127.0.0.2, which the host list does not hold")
		}
		both, err := New(Settings{Brokers: []string{seed}, BrokerHosts: []string{"127.0.0.1", "127.0.0.2"}, Topic: fakeTopic,
			InsecureLoopback: true}, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer both.Close()
		if err, _ := deliver(t, both, 10*time.Second); err != nil {
			t.Fatalf("a send with both hosts in the list: %v", err)
		}
		if n := stored(t, c.ListenAddrs()[0]); n != 1 {
			t.Fatalf("the broker holds %d records, want only the send that the list allowed", n)
		}
		if _, err := New(Settings{Brokers: []string{seed}, BrokerHosts: []string{"127.0.0.2"}, InsecureLoopback: true}, nil); err == nil ||
			!strings.Contains(err.Error(), BrokersSetting) {
			t.Fatalf("a seed broker outside the host list gave %v, want a refusal that names %s", err, BrokersSetting)
		}
	})

	t.Run("ST-SUP-RP01-LC32-T07 A SCRAM challenge out of its bounds is refused before any work", func(t *testing.T) {
		server, ca := certificates(t, "", net.ParseIP("127.0.0.1"))
		password := writeFile(t, "password", "secret-pass")
		for _, c := range []struct{ name, extra string }{
			{"an iteration count of 1000000000", ",i=1000000000"},
			{"two iteration counts", ",i=4096,i=4096"},
			{"a challenge of more than 16 KiB", ",i=4096,e=" + strings.Repeat("x", 17<<10)},
		} {
			cluster := fake(t, on("127.0.0.1"), kfake.TLS(server), kfake.EnableSASL(), kfake.Superuser(MechanismSCRAMSHA512, "svc", "secret-pass"))
			cluster.ControlKey(int16(kmsg.SASLAuthenticate), func(req kmsg.Request) (kmsg.Response, error, bool) {
				request := req.(*kmsg.SASLAuthenticateRequest)
				_, nonce, _ := strings.Cut(string(request.SASLAuthBytes), ",r=")
				response := request.ResponseKind().(*kmsg.SASLAuthenticateResponse)
				response.SASLAuthBytes = []byte("r=" + nonce + "server,s=c2FsdA==" + c.extra)
				return response, nil, true
			})
			logs := &recorder{}
			d, err := New(Settings{Brokers: cluster.ListenAddrs(), Topic: fakeTopic, CAFile: ca, Mechanism: MechanismSCRAMSHA512,
				Username: "svc", PasswordFile: password}, logs)
			if err != nil {
				t.Fatal(err)
			}
			err, took := deliver(t, d, 10*time.Second)
			d.Close()
			if err == nil || err.Error() != errSCRAMBounds.Error() || took > 3*time.Second {
				t.Errorf("%s: %v after %v, want %q at once", c.name, err, took, errSCRAMBounds)
			}
			if got := logs.got(); len(got) == 0 || got[0] != FailureSCRAMBounds {
				t.Errorf("%s: logged %v, want %s", c.name, got, FailureSCRAMBounds)
			}
		}
	})

	t.Run("ST-SUP-RP01-LC32-T09 A SASL refusal on the connection of the produce is classified, not only on the ping", func(t *testing.T) {
		server, ca := certificates(t, "", net.ParseIP("127.0.0.1"))
		cluster := fake(t, on("127.0.0.1"), kfake.TLS(server), kfake.EnableSASL(), kfake.Superuser(MechanismSCRAMSHA512, "svc", "secret-pass"))
		// The broker takes the first authentication, the connection of the metadata ping, and refuses each later one,
		// so the produce opens its own connection and the broker refuses it.
		var mu sync.Mutex
		authentications := 0
		cluster.ControlKey(int16(kmsg.SASLAuthenticate), func(req kmsg.Request) (kmsg.Response, error, bool) {
			cluster.KeepControl()
			mu.Lock()
			defer mu.Unlock()
			if request := req.(*kmsg.SASLAuthenticateRequest); strings.Contains(string(request.SASLAuthBytes), "n,,") {
				authentications++
			}
			if authentications <= 1 {
				return nil, nil, false
			}
			response := req.ResponseKind().(*kmsg.SASLAuthenticateResponse)
			response.ErrorCode = kerr.SaslAuthenticationFailed.Code
			return response, nil, true
		})
		logs := &recorder{}
		d, err := New(Settings{Brokers: cluster.ListenAddrs(), Topic: fakeTopic, CAFile: ca, Mechanism: MechanismSCRAMSHA512,
			Username: "svc", PasswordFile: writeFile(t, "password", "secret-pass")}, logs)
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		err, _ = deliver(t, d, 5*time.Second)
		if err == nil || outbox.IsPermanent(err) || strings.Contains(err.Error(), "secret-pass") {
			t.Fatalf("a refused authentication of the produce gave %v, want an error that is not permanent and holds no password", err)
		}
		if got := logs.got(); len(got) == 0 || got[0] != FailureSASL {
			t.Fatalf("logged %v, want one event of the class %s", got, FailureSASL)
		}
		mu.Lock()
		defer mu.Unlock()
		if authentications < 2 {
			t.Fatalf("the broker saw %d authentications, want the ping's and the produce's", authentications)
		}
	})

	t.Run("ST-SUP-RP01-LC32-T13 A failure class that a connection of an earlier attempt left does not name a later failure", func(t *testing.T) {
		cluster := fake(t, on("127.0.0.1"))
		// The broker holds back the answer of each produce longer than the attempt, so the attempt ends at its
		// deadline with an outcome that is not known and a class of no connection.
		cluster.ControlKey(int16(kmsg.Produce), func(kmsg.Request) (kmsg.Response, error, bool) {
			cluster.KeepControl()
			time.Sleep(3 * time.Second)
			return nil, nil, false
		})
		logs := &recorder{}
		d, err := New(Settings{Brokers: cluster.ListenAddrs(), Topic: fakeTopic, InsecureLoopback: true}, logs)
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		s, err := d.acquire()
		if err != nil {
			t.Fatal(err)
		}
		// A connection of an earlier attempt failed its SASL authentication, and that attempt ended well.
		s.connects.OnBrokerConnect(kgo.BrokerMetadata{}, 0, nil, errSASL)
		err, _ = deliver(t, d, 2*time.Second)
		if err == nil || err.Error() == errSASL.Error() {
			t.Fatalf("the held produce gave %v, want an outcome that is not known and not the SASL refusal of an earlier attempt", err)
		}
		if got := logs.got(); len(got) != 0 {
			t.Fatalf("logged %v, want no event of an earlier attempt's class", got)
		}
	})

	t.Run("ST-SUP-RP01-LC32-T14 The insecure loopback mode logs a warning when the destination is made", func(t *testing.T) {
		events := &eventNames{}
		d, err := New(Settings{Brokers: []string{"127.0.0.1:9092"}, InsecureLoopback: true}, events)
		if err != nil {
			t.Fatal(err)
		}
		d.Close()
		tls, err := New(Settings{Brokers: []string{"k:9093"}, Mechanism: MechanismOAuthBearer, TokenFile: "/run/token"}, events)
		if err != nil {
			t.Fatal(err)
		}
		tls.Close()
		if got := events.got(); len(got) != 1 || got[0] != "kafka.insecure_loopback WARN" {
			t.Fatalf("logged %v, want one warning kafka.insecure_loopback for the destination in the insecure loopback mode only", got)
		}
	})

	t.Run("ST-SUP-RP01-LC32-T08 A certificate from the CA for another host name is refused", func(t *testing.T) {
		server, ca := certificates(t, "other.example", nil)
		cluster := fake(t, on("127.0.0.1"), kfake.TLS(server))
		logs := &recorder{}
		d, err := New(Settings{Brokers: cluster.ListenAddrs(), Topic: fakeTopic, CAFile: ca, Mechanism: MechanismOAuthBearer,
			TokenFile: writeFile(t, "token", "token")}, logs)
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		if err, _ := deliver(t, d, 10*time.Second); err == nil || err.Error() != errTLSVerify.Error() {
			t.Fatalf("a broker whose certificate names another host gave %v, want %q", err, errTLSVerify)
		}
		if got := logs.got(); len(got) == 0 || got[0] != FailureTLSVerification {
			t.Fatalf("logged %v, want %s", got, FailureTLSVerification)
		}
		// The same broker with a certificate for its address is taken, so the refusal is the host name's.
		good, goodCA := certificates(t, "", net.ParseIP("127.0.0.1"))
		ok := fake(t, on("127.0.0.1"), kfake.TLS(good), kfake.EnableSASL(), kfake.Superuser(MechanismSCRAMSHA512, "svc", "secret-pass"))
		d2, err := New(Settings{Brokers: ok.ListenAddrs(), Topic: fakeTopic, CAFile: goodCA, Mechanism: MechanismSCRAMSHA512,
			Username: "svc", PasswordFile: writeFile(t, "password", "secret-pass")}, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer d2.Close()
		if err, _ := deliver(t, d2, 10*time.Second); err != nil {
			t.Fatalf("a broker whose certificate names its address: %v", err)
		}
	})

	t.Run("ST-SUP-RP01-LC31-T05 An answer that announces a huge size is refused at once", func(t *testing.T) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		go func() {
			for {
				conn, err := l.Accept()
				if err != nil {
					return
				}
				go func() {
					defer conn.Close()
					var size [4]byte
					if _, err := io.ReadFull(conn, size[:]); err != nil {
						return
					}
					// An answer of 48 MiB is announced and never sent.
					_, _ = conn.Write([]byte{0x03, 0x00, 0x00, 0x00, 0, 0, 0, 0})
					_, _ = io.Copy(io.Discard, conn)
				}()
			}
		}()
		d, err := New(Settings{Brokers: []string{l.Addr().String()}, Topic: fakeTopic, InsecureLoopback: true}, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		err, took := deliver(t, d, 10*time.Second)
		if err == nil || err.Error() != errUnavailable.Error() {
			t.Fatalf("a hostile answer size gave %v, want %q", err, errUnavailable)
		}
		if took > time.Second {
			t.Fatalf("the destination waited %v for the announced answer, want a refusal at once", took)
		}
	})
}
