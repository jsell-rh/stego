package sample

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	client "example.com/grpc-test/out/grpcapi/client"
	pb "example.com/grpc-test/out/grpcapi/pb/sample/v1"
	tracing "example.com/grpc-test/out/telemetry"
	collector "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type clientTraceSink struct {
	collector.UnimplementedTraceServiceServer
	received chan *collector.ExportTraceServiceRequest
}

func (c *clientTraceSink) Export(ctx context.Context, r *collector.ExportTraceServiceRequest) (*collector.ExportTraceServiceResponse, error) {
	select {
	case c.received <- r:
		return &collector.ExportTraceServiceResponse{}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type tracedClientService struct {
	pb.UnimplementedRecordsServer
	received chan metadata.MD
}

func (s *tracedClientService) capture(ctx context.Context) {
	md, _ := metadata.FromIncomingContext(ctx)
	s.received <- md.Copy()
}
func (s *tracedClientService) Echo(ctx context.Context, r *pb.Request) (*pb.Response, error) {
	s.capture(ctx)
	if r.Text == "wait" {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return &pb.Response{}, nil
}
func (s *tracedClientService) Watch(r *pb.Request, stream grpc.ServerStreamingServer[pb.Response]) error {
	s.capture(stream.Context())
	if r.Text == "hold-headers" {
		<-stream.Context().Done()
		return stream.Context().Err()
	}
	if r.Text == "fail" {
		return status.Error(codes.Unavailable, "private-provider-error")
	}
	if err := stream.Send(&pb.Response{}); err != nil {
		return err
	}
	if r.Text == "hold" {
		<-stream.Context().Done()
		return stream.Context().Err()
	}
	return nil
}
func TestGeneratedClientTracesCompleteCallsAndStreams(t *testing.T) {
	certificate := httptest.NewTLSServer(http.NotFoundHandler())
	defer certificate.Close()
	dir := t.TempDir()
	caFile, tokenFile := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "token")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenFile, []byte("private-client-token"), 0600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, Certificates: certificate.TLS.Certificates})))
	sink := &clientTraceSink{received: make(chan *collector.ExportTraceServiceRequest, 32)}
	service := &tracedClientService{received: make(chan metadata.MD, 16)}
	collector.RegisterTraceServiceServer(server, sink)
	pb.RegisterRecordsServer(server, service)
	go server.Serve(listener)
	defer server.Stop()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://"+listener.Addr().String())
	t.Setenv("OTEL_EXPORTER_OTLP_CERTIFICATE", caFile)
	t.Setenv("OTEL_TRACES_SAMPLER_ARG", "1")
	t.Setenv("OTEL_LOGS_EXPORTER", "none")
	t.Setenv("OTEL_METRICS_EXPORTER", "none")
	runtime, err := tracing.NewRuntime()
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	connection, err := client.New(client.Options{Address: listener.Addr().String(), CAFile: caFile, TokenFile: tokenFile})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	api := pb.NewRecordsClient(connection)
	parent, cancel := context.WithTimeout(runtime.Context(context.Background()), 10*time.Second)
	defer cancel()
	ctx := metadata.NewOutgoingContext(parent, metadata.Pairs("traceparent", "private-caller-parent", "tracestate", "private-caller-state", "baggage", "private-caller-baggage"))
	if _, err := api.Echo(ctx, &pb.Request{Text: "private-request-data"}); err != nil {
		t.Fatal(err)
	}
	timed, stopTimed := context.WithTimeout(ctx, time.Second)
	if _, err := api.Echo(timed, &pb.Request{Text: "wait"}); status.Code(err) != codes.DeadlineExceeded {
		t.Fatal("unary deadline changed", err)
	}
	stopTimed()
	held, cancelHeld := context.WithCancel(ctx)
	stream, err := api.Watch(held, &pb.Request{Text: "hold"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatal(err)
	}
	spans := map[string]*tracepb.Span{}
	collect := func(batch *collector.ExportTraceServiceRequest) {
		data, _ := proto.Marshal(batch)
		if strings.Contains(string(data), "private-") {
			t.Fatal("client trace exposed private data")
		}
		for _, resource := range batch.ResourceSpans {
			for _, scope := range resource.ScopeSpans {
				for _, span := range scope.Spans {
					spans[hex.EncodeToString(span.SpanId)] = span
				}
			}
		}
	}
	// A received frame must not end the stream span.
	timer := time.NewTimer(250 * time.Millisecond)
waiting:
	for {
		select {
		case batch := <-sink.received:
			collect(batch)
		case <-timer.C:
			break waiting
		}
	}
	for _, span := range spans {
		if span.Name == "sample.v1.Records/Watch" {
			t.Fatal("stream span ended after its first message")
		}
	}
	cancelHeld()
	if _, err := stream.Recv(); status.Code(err) != codes.Canceled {
		t.Fatal("stream cancellation changed", err)
	}
	stream, err = api.Watch(ctx, &pb.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err != io.EOF {
		t.Fatal("stream EOF changed", err)
	}
	stream, err = api.Watch(ctx, &pb.Request{Text: "fail"})
	if err == nil {
		_, err = stream.Recv()
	}
	if status.Code(err) != codes.Unavailable {
		t.Fatal("stream failure changed", err)
	}
	stream, err = api.Watch(ctx, &pb.Request{Text: "hold-headers"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Header(); status.Code(err) != codes.DeadlineExceeded {
		t.Fatal("handshake deadline changed", err)
	}
	if err := connection.Invoke(ctx, "/private.service/private_method", &pb.Request{}, &pb.Response{}); status.Code(err) != codes.Unimplemented {
		t.Fatal("unknown call changed", err)
	}
	if _, err := connection.NewStream(ctx, &grpc.StreamDesc{ClientStreams: true}, "/sample.v1.Records/Watch"); status.Code(err) != codes.Unimplemented {
		t.Fatal("unsupported stream was accepted", err)
	}
	runtime.Close()
	for len(sink.received) > 0 {
		collect(<-sink.received)
	}
	expected := map[string]int{"sample.v1.Records/Echo/OK": 1, "sample.v1.Records/Echo/DEADLINE_EXCEEDED": 1, "sample.v1.Records/Watch/CANCELLED": 1, "sample.v1.Records/Watch/OK": 1, "sample.v1.Records/Watch/UNAVAILABLE": 1, "sample.v1.Records/Watch/UNIMPLEMENTED": 1, "sample.v1.Records/Watch/DEADLINE_EXCEEDED": 1, "_OTHER/UNIMPLEMENTED": 1}
	for _, span := range spans {
		if span.Kind != tracepb.Span_SPAN_KIND_CLIENT {
			t.Fatal("wrong client span kind")
		}
		code := ""
		for _, attr := range span.Attributes {
			if attr.Key == "rpc.response.status_code" {
				code = attr.Value.GetStringValue()
			}
		}
		key := span.Name + "/" + code
		if expected[key] != 1 {
			t.Fatal("unexpected or repeated client span", key)
		}
		expected[key]--
	}
	for key, count := range expected {
		if count != 0 {
			t.Fatal("missing client span", key)
		}
	}
	if len(service.received) != 6 {
		t.Fatal("unexpected provider call count", len(service.received))
	}
	for len(service.received) > 0 {
		md := <-service.received
		values := md.Get("traceparent")
		if len(values) != 1 || len(values[0]) != 55 || len(md.Get("tracestate")) != 0 || len(md.Get("baggage")) != 0 || len(md.Get("authorization")) != 1 || md.Get("authorization")[0] != "Bearer private-client-token" {
			t.Fatal("wire propagation lost metadata isolation")
		}
		if spans[values[0][36:52]] == nil {
			t.Fatal("wire trace context has no client span")
		}
	}
}
