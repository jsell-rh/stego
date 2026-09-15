package sample

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	client "example.com/grpc-test/out/grpcapi/client"
	pb "example.com/grpc-test/out/grpcapi/pb/sample/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type probeService struct {
	pb.UnimplementedRecordsServer
	received chan metadata.MD
}

func (s *probeService) Echo(ctx context.Context, r *pb.Request) (*pb.Response, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	s.received <- md.Copy()
	switch r.Text {
	case "wait":
		<-ctx.Done()
		return nil, ctx.Err()
	case "denied":
		return nil, status.Error(codes.Unauthenticated, "authentication required")
	case "large":
		return &pb.Response{Text: strings.Repeat("x", client.MaxResponseBytes+1)}, nil
	default:
		return &pb.Response{Text: r.Text}, nil
	}
}

func TestTLSProbe(t *testing.T) {
	cert := httptest.NewTLSServer(http.NotFoundHandler())
	defer cert.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	service := &probeService{received: make(chan metadata.MD, 16)}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, Certificates: cert.TLS.Certificates})))
	pb.RegisterRecordsServer(server, service)
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(listener) }()
	defer func() { server.Stop(); <-done }()
	roots := x509.NewCertPool()
	roots.AddCert(cert.Certificate())
	options := client.TLSProbeOptions{Address: listener.Addr().String(), Roots: roots, PeerCertificateSHA256: sha256.Sum256(cert.Certificate().Raw)}
	probe, err := client.NewTLSProbe(options)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	parent, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	ctx := metadata.NewOutgoingContext(parent, metadata.Pairs("authorization", "private-token", "cookie", "private-cookie", "x-private", "private-value", "if-resource-version", "8"))
	var response pb.Response
	if err := probe.Invoke(ctx, "/sample.v1.Records/Echo", &pb.Request{Text: "health"}, &response); err != nil || response.Text != "health" {
		t.Fatal("verified probe failed", err)
	}
	received := <-service.received
	for _, name := range []string{"authorization", "cookie", "x-private", "if-resource-version"} {
		if len(received.Get(name)) != 0 {
			t.Fatal("probe forwarded caller metadata", name)
		}
	}
	original, _ := metadata.FromOutgoingContext(ctx)
	if original.Get("authorization")[0] != "private-token" {
		t.Fatal("probe changed caller metadata")
	}
	if err := probe.Invoke(ctx, "/sample.v1.Records/Echo", &pb.Request{Text: "denied"}, &pb.Response{}); status.Code(err) != codes.Unauthenticated {
		t.Fatal("probe lost denied status", err)
	}
	for _, request := range []string{"large", strings.Repeat("r", client.MaxRequestBytes+1)} {
		if err := probe.Invoke(ctx, "/sample.v1.Records/Echo", &pb.Request{Text: request}, &pb.Response{}); status.Code(err) != codes.ResourceExhausted {
			t.Fatal("probe lost message limit", err)
		}
	}
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	if err := probe.Invoke(short, "/sample.v1.Records/Echo", &pb.Request{Text: "wait"}, &pb.Response{}); status.Code(err) != codes.DeadlineExceeded {
		t.Fatal("probe lost cancellation", err)
	}
	cancel()
	if err := probe.Invoke(nil, "/sample.v1.Records/Echo", &pb.Request{}, &pb.Response{}); status.Code(err) != codes.InvalidArgument {
		t.Fatal("probe accepted nil context", err)
	}
	badPin := options
	badPin.PeerCertificateSHA256[0] ^= 1
	badRoots := options
	badRoots.Roots = x509.NewCertPool()
	_, port, _ := net.SplitHostPort(options.Address)
	if cert.Certificate().VerifyHostname("localhost") == nil {
		t.Fatal("test certificate unexpectedly covers localhost")
	}
	badHost := options
	badHost.Address = net.JoinHostPort("localhost", port)
	for _, bad := range []client.TLSProbeOptions{badPin, badRoots, badHost} {
		p, err := client.NewTLSProbe(bad)
		if err != nil {
			t.Fatal(err)
		}
		err = p.Invoke(parent, "/sample.v1.Records/Echo", &pb.Request{Text: "health"}, &pb.Response{})
		p.Close()
		if status.Code(err) != codes.Unavailable {
			t.Fatal("probe accepted a different certificate, trust, or hostname", err)
		}
	}
	for _, bad := range []client.TLSProbeOptions{{Address: options.Address, Roots: roots}, {Address: options.Address, PeerCertificateSHA256: options.PeerCertificateSHA256}, {Address: "https://invalid", Roots: roots, PeerCertificateSHA256: options.PeerCertificateSHA256}} {
		if p, err := client.NewTLSProbe(bad); err == nil {
			p.Close()
			t.Fatal("invalid probe configuration accepted")
		}
	}
	if c, err := client.New(client.Options{Address: options.Address}); err == nil {
		c.Close()
		t.Fatal("ordinary client no longer requires credentials")
	}
}
