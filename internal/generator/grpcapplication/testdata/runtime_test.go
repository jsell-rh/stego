package sample

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	pb "example.com/grpc-test/out/grpcapi/pb/sample/v1"
	transport "example.com/grpc-test/out/grpcapi/transport"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func TestRuntime(t *testing.T) {
	optional := &pb.Request{Label: proto.String("")}
	encoded, err := proto.Marshal(optional)
	if err != nil {
		t.Fatal(err)
	}
	var decoded pb.Request
	if err := proto.Unmarshal(encoded, &decoded); err != nil || decoded.Label == nil || *decoded.Label != "" {
		t.Fatalf("optional presence changed: %v", err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	dir := t.TempDir()
	for name, data := range map[string][]byte{"cert.pem": certPEM, "key.pem": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("STEGO_GRPC_ADDR", "127.0.0.1:0")
	t.Setenv("STEGO_GRPC_TLS_CERT", filepath.Join(dir, "cert.pem"))
	t.Setenv("STEGO_GRPC_TLS_KEY", filepath.Join(dir, "key.pem"))
	authenticate := func(ctx context.Context, token string) (context.Context, error) {
		if token != "good" {
			return nil, errors.New("private authentication error")
		}
		return context.WithValue(ctx, identityKey{}, "alice"), nil
	}
	runtime, err := transport.New(authenticate, func(r grpc.ServiceRegistrar) error { return Register(r, nil) })
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(certPEM)
	connection, err := grpc.NewClient(runtime.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots})))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	client := pb.NewRecordsClient(connection)
	calls, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	authorized := metadata.NewOutgoingContext(calls, metadata.Pairs("authorization", "Bearer good"))
	for _, item := range []struct {
		ctx  context.Context
		text string
		want codes.Code
	}{
		{authorized, "hello", codes.OK}, {calls, "hello", codes.Unauthenticated},
		{metadata.NewOutgoingContext(calls, metadata.Pairs("authorization", "Bearer bad")), "hello", codes.Unauthenticated},
		{metadata.NewOutgoingContext(calls, metadata.Pairs("authorization", "Bearer good", "authorization", "Bearer good")), "hello", codes.Unauthenticated},
		{authorized, "private", codes.Internal}, {authorized, "panic", codes.Internal}, {authorized, strings.Repeat("x", transport.MaxRequestBytes+1), codes.ResourceExhausted},
	} {
		result, err := client.Echo(item.ctx, &pb.Request{Text: item.text})
		if status.Code(err) != item.want {
			t.Fatalf("code=%v want=%v: %v", status.Code(err), item.want, err)
		}
		if err == nil && result.Text != item.text {
			t.Fatal("response changed")
		}
		if err != nil && strings.Contains(err.Error(), "private") {
			t.Fatalf("private error escaped: %v", err)
		}
	}
	stream, err := client.Watch(authorized, &pb.Request{})
	if err != nil {
		t.Fatal(err)
	}
	event, err := stream.Recv()
	if err != nil || event.Text != "event" {
		t.Fatalf("stream identity: %v %v", event, err)
	}
	denied, err := client.Watch(calls, &pb.Request{})
	if err == nil {
		_, err = denied.Recv()
	}
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("stream authentication: %v", err)
	}
	deadline, deadlineCancel := context.WithTimeout(authorized, 30*time.Millisecond)
	defer deadlineCancel()
	if _, err := client.Echo(deadline, &pb.Request{Text: "wait"}); status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("deadline: %v", err)
	}
	var wait sync.WaitGroup
	for i := 0; i < 16; i++ {
		wait.Go(func() {
			if _, err := client.Echo(authorized, &pb.Request{Text: "parallel"}); err != nil {
				t.Error(err)
			}
		})
	}
	wait.Wait()
	untrusted, err := grpc.NewClient(runtime.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{RootCAs: x509.NewCertPool(), MinVersion: tls.VersionTLS13})))
	if err != nil {
		t.Fatal(err)
	}
	defer untrusted.Close()
	if _, err := pb.NewRecordsClient(untrusted).Echo(authorized, &pb.Request{}); status.Code(err) != codes.Unavailable {
		t.Fatalf("untrusted TLS: %v", err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("runtime did not stop")
	}
	runtime.Close()
	runtime.Close()
	if err := runtime.Run(context.Background()); err == nil {
		t.Fatal("runtime ran after close")
	}
	t.Setenv("STEGO_GRPC_TLS_KEY", "")
	if _, err := transport.New(authenticate, func(grpc.ServiceRegistrar) error { return nil }); err == nil {
		t.Fatal("runtime accepted absent TLS")
	}
}
