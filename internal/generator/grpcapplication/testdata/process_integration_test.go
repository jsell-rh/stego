package integration

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	pb "example.com/grpc-test/out/grpcapi/pb/sample/v1"
	"github.com/golang-jwt/jwt/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type output struct {
	sync.Mutex
	b bytes.Buffer
}

func (o *output) Write(p []byte) (int, error) { o.Lock(); defer o.Unlock(); return o.b.Write(p) }
func (o *output) String() string              { o.Lock(); defer o.Unlock(); return o.b.String() }
func TestGeneratedProcess(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "records")
	build := exec.Command("go", "build", "-race", "-mod=readonly", "-o", binary, "../out/grpcapi/processes/records")
	if data, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, data)
	}
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	public, err := x509.MarshalPKIXPublicKey(&rsaKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	tlsKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &tlsKey.PublicKey, tlsKey)
	if err != nil {
		t.Fatal(err)
	}
	private, err := x509.MarshalPKCS8PrivateKey(tlsKey)
	if err != nil {
		t.Fatal(err)
	}
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	for name, data := range map[string][]byte{"auth.pem": pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: public}), "tls.pem": cert, "tls.key": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private})} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(cert) {
		t.Fatal("TLS fixture")
	}
	collector, telemetry := newProcessCollector(t, dir)
	var tokens []string
	token := func(subject, audience string, lifetime time.Duration) string {
		t.Helper()
		signed, err := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{"iss": "https://issuer.example", "aud": audience, "sub": subject, "exp": time.Now().Add(lifetime).Unix(), "iat": time.Now().Unix()}).SignedString(rsaKey)
		if err != nil {
			t.Fatal(err)
		}
		tokens = append(tokens, signed)
		return signed
	}
	for _, mode := range []string{"normal", "restart", "open-error", "open-panic", "open-goexit", "register-error", "close-error", "close-panic", "close-goexit"} {
		t.Run(mode, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			address := listener.Addr().String()
			listener.Close()
			monitor, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			monitorAddress := monitor.Addr().String()
			monitor.Close()
			closed := filepath.Join(dir, "closed-"+mode)
			var logs output
			command := exec.Command(binary)
			command.Env = append(os.Environ(), "STEGO_RPC_MONITOR_ADDR="+monitorAddress, "PROCESS_TEST_MODE="+mode, "PROCESS_TEST_CLOSED="+closed, "STEGO_AUTH_PUBLIC_KEY_FILE="+filepath.Join(dir, "auth.pem"), "STEGO_AUTH_ISSUER=https://issuer.example", "STEGO_AUTH_AUDIENCE=records", "STEGO_GRPC_ADDR="+address, "STEGO_GRPC_TLS_CERT="+filepath.Join(dir, "tls.pem"), "STEGO_GRPC_TLS_KEY="+filepath.Join(dir, "tls.key"), "OTEL_SERVICE_NAME=records-rpc", "DATABASE_URL=invalid-no-database-required")
			command.Env = append(command.Env, telemetry...)
			command.Stdout = &logs
			command.Stderr = &logs
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- command.Wait() }()
			exited := false
			t.Cleanup(func() {
				if !exited {
					command.Process.Kill()
					<-done
				}
				if strings.Contains(logs.String(), "private-process") {
					t.Error("private callback error entered logs")
				}
			})
			startupFailure := strings.HasPrefix(mode, "open-") || mode == "register-error"
			if !startupFailure {
				connection, err := grpc.NewClient(address, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13})))
				if err != nil {
					t.Fatal(err)
				}
				defer connection.Close()
				client := pb.NewRecordsClient(connection)
				call := func(subject, audience string) (*pb.Response, error) {
					ctx, cancel := context.WithTimeout(context.Background(), time.Second)
					defer cancel()
					ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token(subject, audience, time.Minute))
					return client.Echo(ctx, &pb.Request{Text: "hello"})
				}
				deadline := time.Now().Add(8 * time.Second)
				for {
					response, err := call("alice", "records")
					if err == nil && response.Text == "hello" {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("RPC process did not serve", status.Code(err))
					}
					time.Sleep(50 * time.Millisecond)
				}
				for _, probe := range []string{"live", "ready"} {
					check := exec.Command(binary, "--stego-probe="+probe)
					check.Env = []string{"STEGO_RPC_MONITOR_ADDR=" + monitorAddress}
					if data, err := check.CombinedOutput(); err != nil {
						t.Fatalf("probe failed: %v %s", err, data)
					}
				}
				if _, err := call("bob", "records"); status.Code(err) != codes.PermissionDenied {
					t.Fatal("domain policy bypassed", status.Code(err))
				}
				if _, err := call("alice", "other"); status.Code(err) != codes.Unauthenticated {
					t.Fatal("audience bypassed", status.Code(err))
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				if _, err := client.Echo(ctx, &pb.Request{}); status.Code(err) != codes.Unauthenticated {
					t.Fatal("missing bearer accepted", status.Code(err))
				}
				cancel()
				if mode == "normal" {
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token("alice", "records", 3*time.Second))
					stream, err := client.Watch(ctx, &pb.Request{Text: "watch"})
					if err != nil {
						t.Fatal(err)
					}
					first, err := stream.Recv()
					if err != nil || first.Text != "watch" {
						t.Fatal("stream lost identity", err)
					}
					if _, err := stream.Recv(); status.Code(err) != codes.Unauthenticated {
						t.Fatal("stream outlived token", status.Code(err))
					}
					cancel()
					ctx, cancel = context.WithTimeout(context.Background(), time.Second)
					ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token("alice", "records", time.Minute))
					if _, err := client.Echo(ctx, &pb.Request{Text: "private"}); status.Code(err) != codes.Internal || strings.Contains(err.Error(), "private-process") {
						t.Fatal("handler error was not isolated")
					}
					cancel()
				}
				if err := command.Process.Signal(syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
			}
			var processErr error
			select {
			case processErr = <-done:
				exited = true
			case <-time.After(8 * time.Second):
				t.Fatal("process did not stop")
			}
			wantFailure := mode != "normal" && mode != "restart"
			if (processErr != nil) != wantFailure {
				t.Fatal("unexpected process result", mode, processErr)
			}
			if wantFailure && !strings.Contains(logs.String(), `"event.name":"rpc.process.failed"`) {
				t.Fatal("safe failure event missing")
			}
			if !strings.HasPrefix(mode, "open-") {
				data, err := os.ReadFile(closed)
				if err != nil || string(data) != "closed" {
					t.Fatal("domain resources were not closed")
				}
			}

		})
	}
	collector.check(t, tokens)
}
