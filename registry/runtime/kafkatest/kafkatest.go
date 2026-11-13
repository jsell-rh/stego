// Package kafkatest gives the Kafka tests of one test binary their local broker (ST-STD-LOCAL-R04): a test of the
// runtime module, or a test in the module of a generated service, which reads the records that the service publishes
// through the Kafka destination of the outbox. It is test support: only test files import it, and no generated
// service code does.
//
// A package's TestMain calls Wrap around its other test main. A test calls Start. The broker is one container of
// Image per test binary, which the first test that needs it starts and which Wrap removes when the tests end. It runs
// with podman when podman is on PATH and can start it, else with docker, without privilege or capabilities, as the
// test's own user, with 512 MiB of memory, one CPU and at most 512 processes, a read-only root file system and its
// data on a tmpfs, its ports published on 127.0.0.1 only, and with a time limit (the binary's
// -test.timeout and one minute, else one hour), so that it stops also when the test binary is killed.
//
// The broker has four listeners, each reached through a proxy of this package on 127.0.0.1, whose address the
// broker advertises: Plain, plaintext with no authentication, whose proxy a test can cut to make an outage or a lost
// answer; Admin, plaintext with no authentication, for the test's own reads and setup; TLS, which requires a client
// certificate that the test CA signed; and SASL, TLS with SCRAM-SHA-512 and OAUTHBEARER, which takes any unsecured
// OAUTHBEARER token that has a subject and has not expired (Kafka's default validator), so a test makes its own.
//
// When neither podman nor docker is on PATH, each test that needs the broker fails: a missing result is not a pass.
// Only when STEGO_TEST_NO_CONTAINERS=1 is set does such a test skip.
package kafkatest

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha512"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"
)

// Image is the pinned Kafka broker image: apache/kafka:4.1.1, by the digest of its multi-platform index. The JVM
// image, not kafka-native, because the native image cannot load the OAUTHBEARER login module.
const Image = "docker.io/apache/kafka:4.1.1@sha256:0bc1bb2478f45b6cea78864df86acdc11e8df2c5172477819a4d12942cbe5d40"

const (
	// NoContainersVariable set to 1 makes a test that needs the broker skip, instead of fail, when no container can
	// start.
	NoContainersVariable = "STEGO_TEST_NO_CONTAINERS"
	// startLimit is the time the container has to pull its image and become ready.
	startLimit = 3 * time.Minute
	// defaultLife is the time limit of the container when the test binary has no -test.timeout.
	defaultLife = time.Hour
)

// The listeners of the broker, by their port in the container.
const (
	plainPort = "9092"
	adminPort = "9093"
	tlsPort   = "9094"
	saslPort  = "9095"
)

// Broker is the local broker of the test binary.
type Broker struct {
	// Plain is the proxy of the plaintext listener with no authentication. A test cuts it to make an outage.
	Plain *Proxy
	// Admin is the address of the plaintext listener for the test's own reads and setup.
	Admin string
	// TLS is the address of the TLS listener that requires a client certificate signed by the test CA.
	TLS string
	// SASL is the address of the TLS listener with SCRAM-SHA-512 and OAUTHBEARER.
	SASL string
	// CAFile is the PEM file of the CA that signed the broker's certificate and ClientCertFile; ClientCertFile and
	// ClientKeyFile are a client certificate and key that the TLS listener takes; OtherCAFile is a CA that signed
	// nothing the broker holds.
	CAFile, ClientCertFile, ClientKeyFile, OtherCAFile string
	// Dir is the folder of the files of the broker, removed when the tests end.
	Dir string

	admin *kgo.Client
}

var (
	once    sync.Once
	broker  *Broker
	skipWhy string
	failWhy string

	mu        sync.Mutex
	tool      string
	container string
	proxies   []*Proxy
	dir       string
)

// Wrap runs main, the test main of the package, and then removes the container and the files of the broker, if a test
// started it, and gives the exit code of main.
//
//	func TestMain(m *testing.M) { os.Exit(kafkatest.Wrap(func() int { return pgtest.Main(m) })) }
func Wrap(main func() int) int {
	code := main()
	remove()
	return code
}

// Start gives the broker, and starts it the first time a test of the binary asks. It fails the test when the broker
// cannot start, and skips it only as the package comment says.
func Start(t testing.TB) *Broker {
	t.Helper()
	once.Do(start)
	switch {
	case broker != nil:
		return broker
	case skipWhy != "":
		t.Skip("kafkatest: " + skipWhy)
	default:
		t.Fatal("kafkatest: " + failWhy)
	}
	return nil
}

func start() {
	var found []string
	for _, name := range []string{"podman", "docker"} {
		if p, err := exec.LookPath(name); err == nil {
			found = append(found, p)
		}
	}
	if len(found) == 0 {
		why := "neither podman nor docker is on PATH, so the Kafka container of the test cannot start"
		if os.Getenv(NoContainersVariable) == "1" {
			skipWhy = why + " (" + NoContainersVariable + "=1)"
			return
		}
		failWhy = why + ". Set " + NoContainersVariable + "=1 to skip these tests instead"
		return
	}
	var errs []string
	for _, path := range found {
		b, err := run(path)
		if err == nil {
			broker = b
			return
		}
		remove()
		errs = append(errs, err.Error())
	}
	failWhy = "start the Kafka container: " + strings.Join(errs, "; then: ")
}

// run writes the files of the broker, starts the proxies and the container, and waits until the broker answers.
func run(path string) (*Broker, error) {
	ctx, cancel := context.WithTimeout(context.Background(), startLimit)
	defer cancel()
	folder, err := os.MkdirTemp("", "stego-kafkatest-")
	if err != nil {
		return nil, err
	}
	mu.Lock()
	dir = folder
	mu.Unlock()
	b := &Broker{Dir: folder}
	if err := b.writeFiles(); err != nil {
		return nil, err
	}
	// The proxies listen first, so the broker can advertise their addresses.
	names := []string{plainPort, adminPort, tlsPort, saslPort}
	byPort := map[string]*Proxy{}
	for _, port := range names {
		p, err := newProxy(port == plainPort)
		if err != nil {
			return nil, err
		}
		mu.Lock()
		proxies = append(proxies, p)
		mu.Unlock()
		byPort[port] = p
	}
	b.Plain, b.Admin, b.TLS, b.SASL = byPort[plainPort], byPort[adminPort].Addr(), byPort[tlsPort].Addr(), byPort[saslPort].Addr()
	properties := strings.Join([]string{
		"process.roles=broker,controller",
		"node.id=1",
		"controller.quorum.voters=1@localhost:9097",
		"controller.listener.names=CONTROLLER",
		"listeners=PLAIN://:" + plainPort + ",ADMIN://:" + adminPort + ",TLS://:" + tlsPort + ",SASL://:" + saslPort +
			",INTERNAL://:9096,CONTROLLER://:9097",
		"advertised.listeners=PLAIN://" + b.Plain.Addr() + ",ADMIN://" + b.Admin + ",TLS://" + b.TLS + ",SASL://" + b.SASL +
			",INTERNAL://localhost:9096",
		"listener.security.protocol.map=PLAIN:PLAINTEXT,ADMIN:PLAINTEXT,TLS:SSL,SASL:SASL_SSL,INTERNAL:PLAINTEXT,CONTROLLER:PLAINTEXT",
		"inter.broker.listener.name=INTERNAL",
		"log.dirs=/var/lib/kafka/data",
		"offsets.topic.replication.factor=1",
		"transaction.state.log.replication.factor=1",
		"transaction.state.log.min.isr=1",
		"group.initial.rebalance.delay.ms=0",
		"auto.create.topics.enable=false",
		"ssl.keystore.type=PEM",
		"ssl.keystore.location=/mnt/shared/config/server.pem",
		"ssl.truststore.type=PEM",
		"ssl.truststore.location=/mnt/shared/config/ca.pem",
		"listener.name.tls.ssl.client.auth=required",
		"listener.name.sasl.sasl.enabled.mechanisms=SCRAM-SHA-512,OAUTHBEARER",
		"listener.name.sasl.scram-sha-512.sasl.jaas.config=org.apache.kafka.common.security.scram.ScramLoginModule required;",
		`listener.name.sasl.oauthbearer.sasl.jaas.config=org.apache.kafka.common.security.oauthbearer.OAuthBearerLoginModule required unsecuredLoginStringClaim_sub="broker";`,
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(folder, "server.properties"), []byte(properties), 0o600); err != nil {
		return nil, err
	}
	life := defaultLife
	if f := flag.Lookup("test.timeout"); f != nil {
		if d, err := time.ParseDuration(f.Value.String()); err == nil && d > 0 {
			life = d + time.Minute
		}
	}
	name := fmt.Sprintf("stego-kafkatest-%d-%s", os.Getpid(), suffix())
	// The broker runs as the test's own user, so it reads the keys of the folder, which only that user may read. Its
	// root file system is read-only, and it writes only to its tmpfs folders.
	args := []string{"run", "--detach", "--rm", "--name", name,
		"--memory", "512m", "--cpus", "1", "--pids-limit", "512",
		"--security-opt", "no-new-privileges", "--cap-drop", "ALL", "--read-only",
		"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"--tmpfs", "/var/lib/kafka/data:rw,size=256m,mode=1777",
		"--tmpfs", "/opt/kafka/config:rw,size=16m,mode=1777",
		"--tmpfs", "/opt/kafka/logs:rw,size=64m,mode=1777",
		// The broker loads the native library of a compression codec from /tmp.
		"--tmpfs", "/tmp:rw,exec,size=64m,mode=1777",
		"--volume", folder + ":/mnt/shared/config:ro",
		"--env", "CLUSTER_ID=" + clusterID(),
		"--env", "KAFKA_HEAP_OPTS=-Xmx256m -Xms256m"}
	for _, port := range names {
		args = append(args, "--publish", "127.0.0.1::"+port)
	}
	if filepath.Base(path) == "podman" {
		// Rootless podman maps the test's user to the same user in the container only with keep-id.
		args = append(args, "--userns", "keep-id")
	}
	args = append(args, "--entrypoint", "timeout", Image, fmt.Sprintf("%ds", int(life.Seconds())), "/etc/kafka/docker/run")
	mu.Lock()
	tool = path
	mu.Unlock()
	out, err := command(ctx, path, args...)
	if err != nil {
		return nil, err
	}
	mu.Lock()
	container = strings.TrimSpace(out)
	mu.Unlock()
	for _, port := range names {
		out, err := command(ctx, path, "port", name, port+"/tcp")
		if err != nil {
			return nil, err
		}
		target := strings.TrimSpace(strings.SplitN(out, "\n", 2)[0])
		if _, _, err := net.SplitHostPort(target); err != nil {
			return nil, fmt.Errorf("read the published port from %q: %w", out, err)
		}
		byPort[port].setTarget(target)
	}
	b.admin, err = kgo.NewClient(kgo.SeedBrokers(b.Admin), kgo.DialTimeout(2*time.Second))
	if err != nil {
		return nil, err
	}
	for {
		check, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := b.admin.Ping(check)
		cancel()
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			logs, _ := command(context.Background(), path, "logs", "--tail", "40", name)
			return nil, fmt.Errorf("the broker is not ready after %v; its last log lines:\n%s", startLimit, logs)
		case <-time.After(250 * time.Millisecond):
		}
	}
	return b, nil
}

// writeFiles writes the test CA, the broker's key and certificate for 127.0.0.1 and localhost, a client certificate,
// and another CA. The folder and the keys are for the test's user only; the broker runs as that user.
func (b *Broker) writeFiles() error {
	caKey, caCert, err := certificate(nil, nil, "stego kafkatest CA", true)
	if err != nil {
		return err
	}
	serverKey, serverCert, err := certificate(caKey, caCert, "localhost", false)
	if err != nil {
		return err
	}
	clientKey, clientCert, err := certificate(caKey, caCert, "stego-outbox", false)
	if err != nil {
		return err
	}
	_, otherCert, err := certificate(nil, nil, "stego kafkatest other CA", true)
	if err != nil {
		return err
	}
	files := map[string][]byte{
		"ca.pem":         pemOf("CERTIFICATE", caCert.Raw),
		"other-ca.pem":   pemOf("CERTIFICATE", otherCert.Raw),
		"server.pem":     append(keyPEM(serverKey), pemOf("CERTIFICATE", serverCert.Raw)...),
		"client.pem":     pemOf("CERTIFICATE", clientCert.Raw),
		"client-key.pem": keyPEM(clientKey),
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(b.Dir, name), data, 0o600); err != nil {
			return err
		}
	}
	b.CAFile, b.OtherCAFile = filepath.Join(b.Dir, "ca.pem"), filepath.Join(b.Dir, "other-ca.pem")
	b.ClientCertFile, b.ClientKeyFile = filepath.Join(b.Dir, "client.pem"), filepath.Join(b.Dir, "client-key.pem")
	return nil
}

// certificate makes a P-256 key and its certificate for one day, signed by parent, or self-signed when parent is nil.
func certificate(parentKey *ecdsa.PrivateKey, parent *x509.Certificate, name string, ca bool) (*ecdsa.PrivateKey, *x509.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		return nil, nil, err
	}
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour)}
	if ca {
		template.IsCA, template.BasicConstraintsValid = true, true
		template.KeyUsage = x509.KeyUsageCertSign | x509.KeyUsageCRLSign
	} else {
		template.KeyUsage = x509.KeyUsageDigitalSignature
		template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}
		template.DNSNames = []string{"localhost"}
		template.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	}
	if parent == nil {
		parent, parentKey = template, key
	}
	der, err := x509.CreateCertificate(rand.Reader, template, parent, &key.PublicKey, parentKey)
	if err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	return key, cert, err
}

func keyPEM(key *ecdsa.PrivateKey) []byte {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		panic(err)
	}
	return pemOf("PRIVATE KEY", der)
}

func pemOf(kind string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der})
}

// CreateTopic creates topic with partitions partitions, or fails the test.
func (b *Broker) CreateTopic(t testing.TB, topic string, partitions int32) {
	t.Helper()
	request := kmsg.NewPtrCreateTopicsRequest()
	entry := kmsg.NewCreateTopicsRequestTopic()
	entry.Topic, entry.NumPartitions, entry.ReplicationFactor = topic, partitions, 1
	request.Topics = append(request.Topics, entry)
	request.TimeoutMillis = 10000
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	response, err := request.RequestWith(ctx, b.admin)
	if err != nil {
		t.Fatalf("kafkatest: create the topic %s: %v", topic, err)
	}
	for _, result := range response.Topics {
		if err := kerr.ErrorForCode(result.ErrorCode); err != nil {
			t.Fatalf("kafkatest: create the topic %s: %v", topic, err)
		}
	}
}

// SetSCRAM sets the SCRAM-SHA-512 password of user, with 4096 iterations, or fails the test.
func (b *Broker) SetSCRAM(t testing.TB, user, password string) {
	t.Helper()
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		t.Fatal(err)
	}
	const iterations = 4096
	salted, err := pbkdf2.Key(sha512.New, password, salt, iterations, sha512.Size)
	if err != nil {
		t.Fatal(err)
	}
	request := kmsg.NewPtrAlterUserSCRAMCredentialsRequest()
	upsert := kmsg.NewAlterUserSCRAMCredentialsRequestUpsertion()
	upsert.Name, upsert.Mechanism, upsert.Iterations, upsert.Salt, upsert.SaltedPassword = user, 2, iterations, salt, salted
	request.Upsertions = append(request.Upsertions, upsert)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	response, err := request.RequestWith(ctx, b.admin)
	if err != nil {
		t.Fatalf("kafkatest: set the SCRAM password of %s: %v", user, err)
	}
	for _, result := range response.Results {
		if err := kerr.ErrorForCode(result.ErrorCode); err != nil {
			t.Fatalf("kafkatest: set the SCRAM password of %s: %v", user, err)
		}
	}
}

// Token gives an unsecured OAUTHBEARER token for subject that expires at expiry.
func Token(subject string, expiry time.Time) string {
	header, _ := json.Marshal(map[string]string{"alg": "none"})
	claims, _ := json.Marshal(map[string]any{"sub": subject, "iat": time.Now().Add(-time.Minute).Unix(), "exp": expiry.Unix()})
	return base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims) + "."
}

// Records reads topic from its start through the Admin listener until it holds at least least records, and then for
// settle more, so a record that comes later is read too. It fails the test when fewer than least come within limit.
func (b *Broker) Records(t testing.TB, topic string, least int, limit, settle time.Duration) []*kgo.Record {
	t.Helper()
	client, err := kgo.NewClient(kgo.SeedBrokers(b.Admin), kgo.ConsumeTopics(topic),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()), kgo.FetchMaxWait(200*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var records []*kgo.Record
	end := time.Now().Add(limit)
	var settled time.Time
	for {
		now := time.Now()
		if len(records) >= least {
			if settled.IsZero() {
				settled = now.Add(settle)
			}
			if now.After(settled) {
				return records
			}
		} else if now.After(end) {
			t.Fatalf("kafkatest: %d records of the topic %s within %v, want at least %d", len(records), topic, limit, least)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		fetches := client.PollFetches(ctx)
		cancel()
		for _, e := range fetches.Errors() {
			if !errors.Is(e.Err, context.DeadlineExceeded) {
				t.Fatalf("kafkatest: read the topic %s: %v", topic, e.Err)
			}
		}
		records = append(records, fetches.Records()...)
	}
}

// Header gives the value of the header key of record, and whether the record has it once.
func Header(record *kgo.Record, key string) (string, bool) {
	var value string
	n := 0
	for _, h := range record.Headers {
		if h.Key == key {
			value = string(h.Value)
			n++
		}
	}
	return value, n == 1
}

// Proxy forwards the TCP connections of one listener to the published port of the container. The proxy of the Plain
// listener reads the Kafka request frames, so a test can lose the answer to the next produce request.
type Proxy struct {
	listener net.Listener
	frames   bool

	mu       sync.Mutex
	target   string
	down     bool
	dropNext bool
	hold     bool
	conns    map[net.Conn]struct{}
	produced int
}

func newProxy(frames bool) (*Proxy, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &Proxy{listener: l, frames: frames, conns: map[net.Conn]struct{}{}}
	go p.serve()
	return p, nil
}

// Addr is the address of the proxy, which the broker advertises.
func (p *Proxy) Addr() string { return p.listener.Addr().String() }

func (p *Proxy) setTarget(target string) {
	p.mu.Lock()
	p.target = target
	p.mu.Unlock()
}

// Down closes every connection of the proxy and refuses new ones until Up: the broker is not reachable.
func (p *Proxy) Down() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.down = true
	for c := range p.conns {
		_ = c.Close()
	}
}

// Up lets the proxy take connections again, and ends Hold.
func (p *Proxy) Up() {
	p.mu.Lock()
	p.down, p.hold = false, false
	p.mu.Unlock()
}

// Hold makes the proxy forward each produce request to the broker and hold back every answer after it on that
// connection, and keep the connection open, until Up: the broker holds the record and its answer never comes.
func (p *Proxy) Hold() {
	p.mu.Lock()
	p.hold = true
	p.mu.Unlock()
}

// LoseNextAnswer makes the proxy forward the next produce request to the broker and then lose its answer: it holds
// the broker's answer back, waits a second so the broker appends the record, and goes down as Down does.
func (p *Proxy) LoseNextAnswer() {
	p.mu.Lock()
	p.dropNext = true
	p.mu.Unlock()
}

// Produced counts the produce requests that the proxy forwarded.
func (p *Proxy) Produced() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.produced
}

func (p *Proxy) close() {
	_ = p.listener.Close()
	p.Down()
}

func (p *Proxy) serve() {
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		go p.handle(client)
	}
}

func (p *Proxy) handle(client net.Conn) {
	p.mu.Lock()
	target, down := p.target, p.down
	p.mu.Unlock()
	if down || target == "" {
		_ = client.Close()
		return
	}
	server, err := net.DialTimeout("tcp", target, 2*time.Second)
	if err != nil {
		_ = client.Close()
		return
	}
	p.mu.Lock()
	if p.down {
		p.mu.Unlock()
		_ = client.Close()
		_ = server.Close()
		return
	}
	p.conns[client], p.conns[server] = struct{}{}, struct{}{}
	p.mu.Unlock()
	muted := &mutedWriter{w: client}
	done := func() {
		_ = client.Close()
		_ = server.Close()
		p.mu.Lock()
		delete(p.conns, client)
		delete(p.conns, server)
		p.mu.Unlock()
	}
	go func() {
		defer done()
		_, _ = io.Copy(muted, server)
	}()
	defer done()
	if !p.frames {
		_, _ = io.Copy(server, client)
		return
	}
	for {
		var size [4]byte
		if _, err := io.ReadFull(client, size[:]); err != nil {
			return
		}
		n := binary.BigEndian.Uint32(size[:])
		if n < 2 || n > 64<<20 {
			return
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(client, body); err != nil {
			return
		}
		produce := binary.BigEndian.Uint16(body[:2]) == 0
		lose := false
		if produce {
			p.mu.Lock()
			p.produced++
			lose, p.dropNext = p.dropNext, false
			if p.hold {
				muted.mute()
			}
			p.mu.Unlock()
		}
		if lose {
			muted.mute()
		}
		if _, err := server.Write(append(size[:], body...)); err != nil {
			return
		}
		if lose {
			time.Sleep(time.Second)
			p.Down()
			return
		}
	}
}

// mutedWriter drops what it gets once it is muted.
type mutedWriter struct {
	mu    sync.Mutex
	w     io.Writer
	muted bool
}

func (m *mutedWriter) mute() {
	m.mu.Lock()
	m.muted = true
	m.mu.Unlock()
}

func (m *mutedWriter) Write(b []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.muted {
		return len(b), nil
	}
	return m.w.Write(b)
}

// remove closes the proxies and the admin client, removes the container, if one started, and the folder. It is safe
// to call more than once.
func remove() {
	mu.Lock()
	path, id, list, folder := tool, container, proxies, dir
	container, proxies, dir = "", nil, ""
	mu.Unlock()
	if broker != nil && broker.admin != nil {
		broker.admin.Close()
	}
	for _, p := range list {
		p.close()
	}
	if id != "" {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if _, err := command(ctx, path, "rm", "--force", id); err != nil {
			fmt.Fprintf(os.Stderr, "kafkatest: remove the Kafka container %s: %v\n", id, err)
		}
	}
	if folder != "" {
		_ = os.RemoveAll(folder)
	}
}

// command runs the tool and gives its standard output, or an error with its standard error.
func command(ctx context.Context, path string, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s %s: %v: %s", path, args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// clusterID is a random KRaft cluster ID: 16 bytes in unpadded base64url.
func clusterID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// suffix is 12 random hexadecimal digits.
func suffix() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
