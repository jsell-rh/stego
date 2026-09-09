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
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	rpcclient "example.com/grpc-test/out/grpcapi/client"
	pb "example.com/grpc-test/out/grpcapi/pb/sample/v1"
	transport "example.com/grpc-test/out/grpcapi/transport"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type subjectKey struct{}

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
	for name, values := range map[string][]string{"STEGO_GRPC_STREAM_TIMEOUT": {"0s", "-1s", "31m", "invalid"}, "STEGO_GRPC_STREAM_IO_TIMEOUT": {"0s", "11s", "invalid"}} {
		for _, value := range values {
			t.Setenv(name, value)
			if _, err := transport.New(func(ctx context.Context, _ string) (context.Context, error) { return ctx, nil }, func(grpc.ServiceRegistrar) error { return nil }); err == nil {
				t.Fatalf("accepted %s=%s", name, value)
			}
		}
		t.Setenv(name, "")
	}
	testResourceCleanup(t)
	authenticate := func(ctx context.Context, token string) (context.Context, error) {
		if token != "good" && !strings.HasPrefix(token, "user-") {
			return nil, errors.New("private authentication error")
		}
		return context.WithValue(context.WithValue(ctx, subjectKey{}, token), identityKey{}, "alice"), nil
	}
	t.Setenv("STEGO_GRPC_STREAM_IO_TIMEOUT", "1s")
	var expiry atomic.Int64
	expiry.Store(time.Now().Add(time.Hour).UnixNano())
	runtime, err := transport.New(authenticate, func(r grpc.ServiceRegistrar) error { return Register(r, nil) }, transport.Options{IdentityInfo: func(ctx context.Context) (string, time.Time) {
		return ctx.Value(subjectKey{}).(string), time.Unix(0, expiry.Load())
	}})
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
	calls, stop := context.WithTimeout(context.Background(), 15*time.Second)
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
	// Preparation errors stop both RPC forms without becoming token failures.
	for _, test := range []struct {
		mode string
		want codes.Code
	}{{"fail", codes.Unavailable}, {"private", codes.Internal}} {
		requestCtx := metadata.AppendToOutgoingContext(authorized, "x-test-prepare", test.mode)
		if _, err := client.Echo(requestCtx, &pb.Request{Text: "hello"}); status.Code(err) != test.want || strings.Contains(err.Error(), "private") {
			t.Fatalf("unary preparation: %v", err)
		}
		before := activeStreams.Load()
		denied, err := client.Watch(requestCtx, &pb.Request{})
		if err == nil {
			_, err = denied.Recv()
		}
		if status.Code(err) != test.want || strings.Contains(err.Error(), "private") || activeStreams.Load() != before {
			t.Fatalf("stream preparation: %v", err)
		}
	}
	before := preparations.Load()
	if _, err := client.Echo(calls, &pb.Request{}); status.Code(err) != codes.Unauthenticated || preparations.Load() != before {
		t.Fatal("unauthenticated request reached preparation")
	}
	waitCtx, waitCancel := context.WithTimeout(metadata.AppendToOutgoingContext(authorized, "x-test-prepare", "wait"), 30*time.Millisecond)
	if _, err := client.Echo(waitCtx, &pb.Request{}); status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("preparation deadline: %v", err)
	}
	waitCancel()
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
	// Long streams have a separate capacity pool from unary calls.
	var cancels []context.CancelFunc
	for range 4 {
		streamCtx, streamCancel := context.WithCancel(authorized)
		cancels = append(cancels, streamCancel)
		held, err := client.Watch(streamCtx, &pb.Request{Text: "hold"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := held.Header(); err != nil {
			t.Fatal(err)
		}
	}
	excess, err := client.Watch(authorized, &pb.Request{Text: "hold"})
	if err == nil {
		_, err = excess.Recv()
	}
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("identity stream limit: %v", err)
	}
	if _, err := client.Echo(authorized, &pb.Request{Text: "still available"}); err != nil {
		t.Fatal("streams exhausted unary capacity", err)
	}
	for _, cancel := range cancels {
		cancel()
	}
	awaitStreams := func() {
		deadline := time.Now().Add(2 * time.Second)
		for activeStreams.Load() != 0 {
			if time.Now().After(deadline) {
				t.Fatal("stream handlers did not exit")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	awaitStreams()
	// Distinct identities share the process limit, while unary calls still work.
	cancels = nil
	for i := range 32 {
		streamCtx, streamCancel := context.WithCancel(metadata.NewOutgoingContext(calls, metadata.Pairs("authorization", fmt.Sprintf("Bearer user-%d", i))))
		cancels = append(cancels, streamCancel)
		held, err := client.Watch(streamCtx, &pb.Request{Text: "hold"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := held.Header(); err != nil {
			t.Fatal(err)
		}
	}
	excess, err = client.Watch(authorized, &pb.Request{Text: "hold"})
	if err == nil {
		_, err = excess.Recv()
	}
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("process stream limit: %v", err)
	}
	if _, err := client.Echo(authorized, &pb.Request{Text: "global capacity"}); err != nil {
		t.Fatal(err)
	}
	for _, cancel := range cancels {
		cancel()
	}
	awaitStreams()
	expiry.Store(time.Now().Add(200 * time.Millisecond).UnixNano())
	expiring, err := client.Watch(authorized, &pb.Request{Text: "hold"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := expiring.Header(); err != nil {
		t.Fatal(err)
	}
	if _, err := expiring.Recv(); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("token expiry did not stop stream: %v", err)
	}
	awaitStreams()
	expiry.Store(time.Now().Add(time.Hour).UnixNano())
	// A peer that reads headers but no messages must not retain a send forever.
	flooded, err := client.Watch(authorized, &pb.Request{Text: "flood"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := flooded.Header(); err != nil {
		t.Fatal(err)
	}
	awaitStreams()
	// Server handler completion does not mean that the client has observed the
	// closed connection. Read the terminal result before testing reconnection.
	for messages := 0; ; messages++ {
		if messages == 32 {
			t.Fatal("slow stream did not reach its terminal result")
		}
		if _, err := flooded.Recv(); err != nil {
			break
		}
	}
	if _, err := client.Echo(authorized, &pb.Request{Text: "after slow peer"}, grpc.WaitForReady(true)); err != nil {
		t.Fatal(err)
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
	// Generated outbound clients require trusted TLS and a private token file.
	tokenFile := filepath.Join(dir, "rpc-token")
	if err := os.WriteFile(tokenFile, []byte("good\n"), 0600); err != nil {
		t.Fatal(err)
	}
	opts := rpcclient.Options{Address: runtime.Addr().String(), CAFile: filepath.Join(dir, "cert.pem"), TokenFile: tokenFile}
	outbound, err := rpcclient.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer outbound.Close()
	outboundClient := pb.NewRecordsClient(outbound)
	if _, err := outboundClient.Echo(calls, &pb.Request{Text: "generated client"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenFile, []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := outboundClient.Echo(calls, &pb.Request{Text: "token rotation"}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("rotated token ignored: %v", err)
	}
	if err := os.WriteFile(tokenFile, []byte("good"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := outboundClient.Echo(calls, &pb.Request{Text: strings.Repeat("x", rpcclient.MaxRequestBytes+1)}, grpc.MaxCallSendMsgSize(1<<20)); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("client request limit: %v", err)
	}
	generatedStream, err := outboundClient.Watch(calls, &pb.Request{})
	if err != nil {
		t.Fatalf("generated watch client: %v", err)
	}
	if event, err := generatedStream.Recv(); err != nil || event.GetText() != "event" {
		t.Fatalf("generated watch response: %v", err)
	}
	if _, err := generatedStream.Recv(); err != io.EOF {
		t.Fatalf("generated watch completion: %v", err)
	}
	streamContext, cancelStreams := context.WithCancel(context.Background())
	defer cancelStreams()
	var clientStreams []grpc.ServerStreamingClient[pb.Response]
	for range rpcclient.MaxConcurrentStreams {
		stream, err := outboundClient.Watch(streamContext, &pb.Request{Text: "hold"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := stream.Header(); err != nil {
			t.Fatal(err)
		}
		deadline, ok := stream.Context().Deadline()
		if !ok || time.Until(deadline) > rpcclient.StreamTimeout {
			t.Fatal("stream has no bounded lifetime")
		}
		clientStreams = append(clientStreams, stream)
	}
	if _, err := outboundClient.Watch(streamContext, &pb.Request{}); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("client stream capacity: %v", err)
	}
	if _, err := outboundClient.Echo(calls, &pb.Request{}); err != nil {
		t.Fatalf("stream capacity blocked unary call: %v", err)
	}
	cancelStreams()
	for _, stream := range clientStreams {
		if _, err := stream.Recv(); status.Code(err) != codes.Canceled {
			t.Fatalf("cancel stream: %v", err)
		}
	}
	for _, request := range []string{strings.Repeat("x", rpcclient.MaxRequestBytes+1), "flood"} {
		stream, err := outboundClient.Watch(calls, &pb.Request{Text: request}, grpc.MaxCallSendMsgSize(2<<20), grpc.MaxCallRecvMsgSize(2<<20))
		if err == nil {
			_, err = stream.Recv()
		}
		if status.Code(err) != codes.ResourceExhausted {
			t.Fatalf("stream message limit: %v", err)
		}
	}
	if err := os.WriteFile(tokenFile, []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	denied, err = outboundClient.Watch(calls, &pb.Request{})
	if err == nil {
		_, err = denied.Recv()
	}
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("stream token rotation: %v", err)
	}
	if err := os.WriteFile(tokenFile, []byte("good"), 0600); err != nil {
		t.Fatal(err)
	}
	idleContext, cancelIdle := context.WithCancel(context.Background())
	defer cancelIdle()
	idle, err := outboundClient.Watch(idleContext, &pb.Request{Text: "hold"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := idle.Header(); err != nil {
		t.Fatal(err)
	}
	silent, err := outboundClient.Watch(context.Background(), &pb.Request{Text: "silent"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := silent.Recv(); status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("stream handshake deadline: %v", err)
	}
	if idle.Context().Err() != nil {
		t.Fatal("handshake timer stopped an idle watch")
	}
	cancelIdle()
	if _, err := idle.Recv(); status.Code(err) != codes.Canceled {
		t.Fatalf("idle watch cancellation: %v", err)
	}
	if err := os.Chmod(tokenFile, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := rpcclient.New(opts); err == nil {
		t.Fatal("client accepted public token file")
	}
	if _, err := outboundClient.Echo(calls, &pb.Request{}); err == nil {
		t.Fatal("client ignored changed token permissions")
	}
	if err := os.Chmod(tokenFile, 0600); err != nil {
		t.Fatal(err)
	}
	for _, address := range []string{"", "dns:///localhost:443", "localhost:0", "localhost:65536", "user@localhost:443"} {
		bad := opts
		bad.Address = address
		if _, err := rpcclient.New(bad); err == nil {
			t.Fatalf("unsafe client target: %s", address)
		}
	}
	_, port, _ := net.SplitHostPort(opts.Address)
	wrongHost := opts
	wrongHost.Address = "localhost:" + port
	wrong, err := rpcclient.New(wrongHost)
	if err != nil {
		t.Fatal(err)
	}
	defer wrong.Close()
	if _, err := pb.NewRecordsClient(wrong).Echo(calls, &pb.Request{}); status.Code(err) != codes.Unavailable {
		t.Fatalf("client skipped server identity: %v", err)
	}
	if _, err := outboundClient.Echo(calls, &pb.Request{Text: "unavailable"}); status.Code(err) != codes.Unavailable || unavailableCalls.Load() != 1 {
		t.Fatalf("client replayed an executed call: %v %d", err, unavailableCalls.Load())
	}
	hold, cancelHeld := context.WithCancel(calls)
	var heldCalls sync.WaitGroup
	for range rpcclient.MaxConcurrentCalls {
		heldCalls.Go(func() { _, _ = outboundClient.Echo(hold, &pb.Request{Text: "wait"}) })
	}
	limitDeadline := time.Now().Add(2 * time.Second)
	for activeWaits.Load() != rpcclient.MaxConcurrentCalls {
		if time.Now().After(limitDeadline) {
			cancelHeld()
			heldCalls.Wait()
			t.Fatal("client calls did not reach server")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := outboundClient.Echo(calls, &pb.Request{}); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("outbound capacity: %v", err)
	}
	cancelHeld()
	heldCalls.Wait()
	start := time.Now()
	if _, err := outboundClient.Echo(context.Background(), &pb.Request{Text: "wait"}); status.Code(err) != codes.DeadlineExceeded || time.Since(start) > 6*time.Second {
		t.Fatalf("client deadline: %v %v", err, time.Since(start))
	}
	outbound.Close()
	if _, err := outboundClient.Echo(calls, &pb.Request{}); err == nil {
		t.Fatal("closed client made a call")
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

type capturedRegistrar struct {
	descriptor     *grpc.ServiceDesc
	implementation any
}

func (r *capturedRegistrar) RegisterService(d *grpc.ServiceDesc, implementation any) {
	r.descriptor = d
	r.implementation = implementation
}

func TestPreparationKeepsRegistrationsIndependent(t *testing.T) {
	first, second := &capturedRegistrar{}, &capturedRegistrar{}
	calls := [2]int{}
	for index, target := range []*capturedRegistrar{first, second} {
		wrapped, err := transport.PrepareRegistrar(target, func(ctx context.Context) error {
			if ctx.Value(identityKey{}) != "alice" {
				return errors.New("preparation ran before the interceptor")
			}
			calls[index]++
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		pb.RegisterRecordsServer(wrapped, records{})
	}
	invoke := func(target *capturedRegistrar) {
		t.Helper()
		result, err := target.descriptor.Methods[0].Handler(target.implementation, context.Background(), func(value any) error { value.(*pb.Request).Text = "hello"; return nil }, func(ctx context.Context, request any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			return handler(context.WithValue(ctx, identityKey{}, "alice"), request)
		})
		if err != nil || result.(*pb.Response).Text != "hello" {
			t.Fatal("prepared registration", result, err)
		}
	}
	invoke(second)
	if calls != [2]int{0, 1} {
		t.Fatal("registration changed another callback", calls)
	}
	invoke(first)
	if calls != [2]int{1, 1} {
		t.Fatal("registration shared a mutable descriptor", calls)
	}
	if _, err := transport.PrepareRegistrar(nil, func(context.Context) error { return nil }); err == nil {
		t.Fatal("nil registrar accepted")
	}
	if _, err := transport.PrepareRegistrar(first, nil); err == nil {
		t.Fatal("nil preparation accepted")
	}
}

func testResourceCleanup(t *testing.T) {
	t.Helper()
	authenticate := func(ctx context.Context, _ string) (context.Context, error) { return ctx, nil }
	for _, mode := range []string{"close", "run", "registration-failure", "bind-failure", "panic"} {
		t.Run("resources/"+mode, func(t *testing.T) {
			var calls []int
			var saved grpc.ServiceRegistrar
			if mode == "bind-failure" {
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
				t.Setenv("STEGO_GRPC_ADDR", listener.Addr().String())
			}
			sentinel := errors.New("registration failed")
			var runtime *transport.Runtime
			var err error
			func() {
				defer func() {
					if r := recover(); r != nil && mode != "panic" {
						t.Fatal(r)
					}
				}()
				runtime, err = transport.New(authenticate, func(r grpc.ServiceRegistrar) error {
					r, e := transport.PrepareRegistrar(r, func(context.Context) error { return nil })
					if e != nil {
						return e
					}
					saved = r
					if e := transport.OnClose(r, func() { calls = append(calls, 1) }); e != nil {
						return e
					}
					if e := transport.OnClose(r, func() { calls = append(calls, 2); panic("private cleanup failure") }); e != nil {
						return e
					}
					if mode == "panic" {
						panic(sentinel)
					}
					if mode == "registration-failure" {
						return sentinel
					}
					return nil
				})
			}()
			if mode == "close" || mode == "run" {
				if err != nil || runtime == nil {
					t.Fatal(err)
				}
				if len(calls) != 0 {
					t.Fatal("resource closed during startup")
				}
				if err := transport.OnClose(saved, func() { t.Error("late callback ran") }); err == nil {
					t.Fatal("resource registered after factory returned")
				}
				if mode == "run" {
					ctx, cancel := context.WithCancel(context.Background())
					done := make(chan error, 1)
					go func() { done <- runtime.Run(ctx) }()
					cancel()
					select {
					case err := <-done:
						if err != nil {
							t.Fatal(err)
						}
					case <-time.After(time.Second):
						t.Fatal("runtime did not release resources")
					}
				}
				runtime.Close()
				runtime.Close()
			} else if runtime != nil || mode != "panic" && err == nil {
				t.Fatal("failed startup returned a runtime", err)
			}
			if len(calls) != 2 || calls[0] != 2 || calls[1] != 1 {
				t.Fatal("resource cleanup order or count", calls)
			}
			if err := transport.OnClose(saved, func() { t.Error("late cleanup accepted") }); err == nil {
				t.Fatal("resource registered after startup or close")
			}
		})
	}
	var closed int
	runtime, err := transport.New(authenticate, func(r grpc.ServiceRegistrar) error {
		if err := transport.OnClose(r, nil); err == nil {
			t.Fatal("nil cleanup callback accepted")
		}
		for range 128 {
			if err := transport.OnClose(r, func() { closed++ }); err != nil {
				return err
			}
		}
		return transport.OnClose(r, func() { t.Error("excess cleanup callback was retained") })
	})
	if runtime != nil || err == nil || closed != 128 {
		t.Fatal("cleanup capacity or failed-startup release", closed, err)
	}
	if err := transport.OnClose(grpc.NewServer(), func() {}); err == nil {
		t.Fatal("unmanaged registrar accepted ownership")
	}
}

func TestClientFailureSummaryOmitsPrivateDetails(t *testing.T) {
	for _, err := range []error{status.Error(codes.Internal, "private-token"), errors.New("private-token"), status.Error(codes.Code(999), "private-token")} {
		summary := rpcclient.FailureSummary(err)
		if strings.Contains(summary, "private") || len(summary) > 64 {
			t.Fatal("unsafe failure summary", summary)
		}
	}
	if rpcclient.FailureSummary(status.Error(codes.Internal, "private-token")) != "RPC code = Internal" {
		t.Fatal("protocol code was lost")
	}
	if rpcclient.FailureSummary(nil) != "" {
		t.Fatal("nil error reported as failure")
	}
}
