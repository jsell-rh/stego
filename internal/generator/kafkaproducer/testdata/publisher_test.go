package publisher

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"
)

type testIdentity struct {
	config Config
	server *tls.Config
}

func identity(t *testing.T, hostname string) testIdentity {
	t.Helper()
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	root := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	rootDER, err := x509.CreateCertificate(rand.Reader, root, root, &rootKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	rootPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER})
	makeCert := func(serial int64, client bool) ([]byte, []byte) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		cert := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "Test identity"},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature}
		if client {
			cert.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
		} else {
			cert.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
			if hostname == "localhost" {
				cert.IPAddresses = []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
				cert.DNSNames = []string{"localhost"}
			} else {
				cert.DNSNames = []string{hostname}
			}
		}
		der, err := x509.CreateCertificate(rand.Reader, cert, root, &key.PublicKey, rootKey)
		if err != nil {
			t.Fatal(err)
		}
		keyDER, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	}
	serverCert, serverKey := makeCert(2, false)
	clientCert, clientKey := makeCert(3, true)
	pair, err := tls.X509KeyPair(serverCert, serverKey)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(rootPEM)
	directory := t.TempDir()
	for name, data := range map[string][]byte{"ca.pem": rootPEM, "client.pem": clientCert, "key.pem": clientKey} {
		if err := os.WriteFile(filepath.Join(directory, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return testIdentity{
		config: Config{Topic: "events", Authentication: "mtls", CAFile: filepath.Join(directory, "ca.pem"),
			ClientCertificateFile: filepath.Join(directory, "client.pem"), ClientKeyFile: filepath.Join(directory, "key.pem")},
		server: &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair},
			ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots},
	}
}

func broker(t *testing.T, identity testIdentity, extra ...kfake.Opt) (*kfake.Cluster, Config) {
	t.Helper()
	options := []kfake.Opt{kfake.NumBrokers(1), kfake.TLS(identity.server), kfake.SeedTopics(1, "events")}
	options = append(options, extra...)
	cluster, err := kfake.NewCluster(options...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cluster.Close)
	config := identity.config
	config.Brokers = cluster.ListenAddrs()
	return cluster, config
}

func testRecord() Record {
	return Record{ID: uuid.New(), ResourceKey: "resource-1", Kind: "resource.created", Payload: []byte(`{"value":"one"}`)}
}

func TestMutualTLSPublishRequiresAllReplicaAcknowledgements(t *testing.T) {
	cluster, config := broker(t, identity(t, "localhost"))
	var allAcks atomic.Bool
	cluster.ControlKey(0, func(request kmsg.Request) (kmsg.Response, error, bool) {
		allAcks.Store(request.(*kmsg.ProduceRequest).Acks == -1)
		cluster.DropControl()
		return nil, nil, false
	})
	publisher, err := New(context.Background(), config)
	if err != nil {
		options, optionErr := clientOptions(config)
		t.Logf("client options: %v; validation: %v", optionErr, kgo.ValidateOpts(options...))
		t.Fatal(err)
	}
	defer publisher.Close()
	record := testRecord()
	if err := publisher.Publish(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if !allAcks.Load() {
		t.Fatal("publisher did not require all in-sync replica acknowledgements")
	}
	options, err := clientOptions(config)
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := kgo.NewClient(append(options, kgo.ConsumeTopics("events"), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))...)
	if err != nil {
		t.Fatal(err)
	}
	defer consumer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	records := consumer.PollRecords(ctx, 1).Records()
	if len(records) != 1 || !bytes.Equal(records[0].Value, record.Payload) || string(records[0].Key) != record.ResourceKey {
		t.Fatalf("published record changed: %v", records)
	}
	headers := make(map[string]string)
	for _, header := range records[0].Headers {
		headers[header.Key] = string(header.Value)
	}
	if headers["stego-message-id"] != record.ID.String() || headers["stego-message-kind"] != record.Kind {
		t.Fatal("stable delivery identity is missing")
	}
}

func TestSCRAMAuthenticatesOverTLS(t *testing.T) {
	id := identity(t, "localhost")
	id.server.ClientAuth = tls.NoClientCert
	_, config := broker(t, id, kfake.EnableSASL(), kfake.Superuser("SCRAM-SHA-512", "publisher", "private-password"))
	config.Authentication = "scram-sha-512"
	config.ClientCertificateFile, config.ClientKeyFile = "", ""
	config.Username = "publisher"
	config.PasswordFile = filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(config.PasswordFile, []byte("private-password\n"), 0600); err != nil {
		t.Fatal(err)
	}
	publisher, err := New(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if err := publisher.Publish(context.Background(), testRecord()); err != nil {
		t.Fatal(err)
	}
	publisher.Close()
	if err := os.WriteFile(config.PasswordFile, []byte("wrong-password"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if p, err := New(ctx, config); err == nil {
		p.Close()
		t.Fatal("wrong SCRAM credentials succeeded")
	} else if strings.Contains(err.Error(), "password") {
		t.Fatal("authentication error exposed credential text")
	}
}

func TestTLSRejectsWrongTrustAndHostname(t *testing.T) {
	for _, failure := range []string{"hostname", "trust", "client", "plaintext"} {
		t.Run(failure, func(t *testing.T) {
			id := identity(t, "localhost")
			if failure == "hostname" {
				id = identity(t, "wrong.example")
			}
			if failure == "plaintext" {
				id.server = nil
			}
			_, config := broker(t, id)
			if failure == "trust" {
				config.CAFile = identity(t, "localhost").config.CAFile
			}
			if failure == "client" {
				other := identity(t, "localhost")
				config.ClientCertificateFile, config.ClientKeyFile = other.config.ClientCertificateFile, other.config.ClientKeyFile
			}
			options, err := clientOptions(config)
			if err != nil || kgo.ValidateOpts(options...) != nil {
				t.Fatalf("negative TLS test has invalid client options: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			if p, err := New(ctx, config); err == nil {
				p.Close()
				t.Fatal("unverified connection succeeded")
			}
		})
	}
}

func TestSCRAMRejectsUnboundedServerWork(t *testing.T) {
	for _, challenge := range []string{
		"r=nonce,s=c2FsdA==,i=100001", "r=nonce,s=c2FsdA==,i=2147483647",
		"r=nonce,s=c2FsdA==,i=4095", "r=nonce,s=c2FsdA==,i=4096,i=4096",
		"r=nonce,s=c2FsdA==", strings.Repeat("x", (16<<10)+1),
	} {
		// A missing inner session makes an accidental crypto call fail this test.
		session := &boundedSCRAMSession{first: true}
		if _, _, err := session.Challenge([]byte(challenge)); err == nil {
			t.Fatal("unbounded SCRAM challenge accepted")
		}
	}
}

func TestUncertainProduceClosesClientBeforeRetry(t *testing.T) {
	cluster, config := broker(t, identity(t, "localhost"))
	publisher, err := New(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer publisher.Close()
	before, _ := publisher.acquire()
	var blocked atomic.Bool
	blocked.Store(true)
	cluster.ControlKey(0, func(request kmsg.Request) (kmsg.Response, error, bool) {
		if blocked.Load() {
			cluster.KeepControl()
			return nil, nil, true
		}
		cluster.DropControl()
		return nil, nil, false
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	record := testRecord()
	if err := publisher.Publish(ctx, record); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("uncertain produce returned %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("produce cancellation left the client running")
	}
	blocked.Store(false)
	after, err := publisher.acquire()
	if err != nil || before == after {
		t.Fatal("uncertain producer was reused")
	}
	if err := publisher.Publish(context.Background(), record); err != nil {
		t.Fatal(err)
	}
}

func TestBrokerDenialDoesNotReturnSuccess(t *testing.T) {
	cluster, config := broker(t, identity(t, "localhost"))
	cluster.ControlKey(0, func(request kmsg.Request) (kmsg.Response, error, bool) {
		produce := request.(*kmsg.ProduceRequest)
		cluster.KeepControl()
		response := kmsg.NewPtrProduceResponse()
		response.Version = produce.Version
		for _, topic := range produce.Topics {
			denied := kmsg.NewProduceResponseTopic()
			denied.Topic = topic.Topic
			denied.TopicID = topic.TopicID
			for _, partition := range topic.Partitions {
				result := kmsg.NewProduceResponseTopicPartition()
				result.Partition = partition.Partition
				result.ErrorCode = kerr.TopicAuthorizationFailed.Code
				denied.Partitions = append(denied.Partitions, result)
			}
			response.Topics = append(response.Topics, denied)
		}
		return response, nil, true
	})
	publisher, err := New(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer publisher.Close()
	if err := publisher.Publish(context.Background(), testRecord()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("denied publish returned %v", err)
	}
}

func TestConcurrentPublishAndClose(t *testing.T) {
	_, config := broker(t, identity(t, "localhost"))
	publisher, err := New(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	var running sync.WaitGroup
	for i := 0; i < 16; i++ {
		running.Add(1)
		go func() { defer running.Done(); _ = publisher.Publish(context.Background(), testRecord()) }()
	}
	publisher.Close()
	running.Wait()
	publisher.Close()
	if err := publisher.Publish(context.Background(), testRecord()); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed publisher returned %v", err)
	}
}

func TestInvalidConfigurationAndRecordFailBeforeNetwork(t *testing.T) {
	id := identity(t, "localhost")
	id.config.Brokers = []string{"localhost:9093"}
	for _, change := range []func(*Config){
		func(c *Config) { c.Brokers = nil }, func(c *Config) { c.Brokers = []string{"localhost"} },
		func(c *Config) { c.Brokers = []string{"localhost:0"} }, func(c *Config) { c.Brokers = []string{"user@localhost:9093"} },
		func(c *Config) { c.Brokers = []string{"localhost:9093", "localhost:9093"} },
		func(c *Config) { c.Authentication = "none" }, func(c *Config) { c.Topic = "__internal" },
		func(c *Config) { c.Topic = "../events" }, func(c *Config) { c.ClientKeyFile = "" },
	} {
		config := id.config
		change(&config)
		if _, err := clientOptions(config); err == nil {
			t.Fatal("invalid Kafka configuration was accepted")
		}
	}
	if err := os.Chmod(id.config.ClientKeyFile, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := clientOptions(id.config); err == nil {
		t.Fatal("world-readable secret was accepted")
	}
	if err := os.Chmod(id.config.ClientKeyFile, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(id.config.CAFile, []byte("not a certificate"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := clientOptions(id.config); err == nil {
		t.Fatal("invalid CA file accepted")
	}
	for _, payload := range [][]byte{nil, []byte("{"), []byte("\xff"), bytes.Repeat([]byte("a"), maxPayloadBytes+1)} {
		record := testRecord()
		record.Payload = payload
		if err := validateRecord(record); err == nil {
			t.Fatal("invalid payload accepted")
		}
	}
}
