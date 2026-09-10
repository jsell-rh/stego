package sample

import (
	"errors"
	"io"
	"strings"
	"testing"

	rpc "example.com/grpc-test/out/grpcapi/client"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

type startupStream struct {
	header                metadata.MD
	headerErr, receiveErr error
	headers, receives     int
}

func (s *startupStream) Header() (metadata.MD, error) { s.headers++; return s.header, s.headerErr }
func (s *startupStream) Recv() (string, error)        { s.receives++; return "event", s.receiveErr }

func TestStreamHeaderContract(t *testing.T) {
	good := []rpc.StreamHeader{{Name: "sample-capability", Value: "v1"}, {Name: "sample-scope", Value: "all-v1"}}
	for _, required := range [][]rpc.StreamHeader{
		nil, {{Name: "", Value: "v1"}}, {{Name: "UPPER", Value: "v1"}}, {{Name: "with space", Value: "v1"}},
		{{Name: "private", Value: ""}}, {{Name: "private", Value: "line\nbreak"}}, {{Name: "private", Value: "é"}},
		{{Name: strings.Repeat("a", 257), Value: "v1"}}, {{Name: "private", Value: strings.Repeat("v", 1025)}},
		{good[0], good[0]}, make([]rpc.StreamHeader, 33),
	} {
		stream := &startupStream{}
		if err := rpc.RequireStreamHeaders(stream, required...); !errors.Is(err, rpc.ErrStreamContract) || stream.headers != 0 || stream.receives != 0 {
			t.Fatal("invalid requirements touched stream", err)
		}
	}
	var absent *startupStream
	if err := rpc.RequireStreamHeaders(absent, good...); !errors.Is(err, rpc.ErrStreamContract) {
		t.Fatal("nil stream accepted", err)
	}
	if err := rpc.RequireStreamHeaders[string](nil, good...); !errors.Is(err, rpc.ErrStreamContract) {
		t.Fatal("nil interface accepted", err)
	}
	for _, header := range []metadata.MD{nil, {}, metadata.Pairs("sample-capability", "v1"), metadata.Pairs("sample-capability", "v1", "sample-scope", "all-v1", "sample-scope", "all-v1")} {
		stream := &startupStream{header: header, receiveErr: io.EOF}
		if err := rpc.RequireStreamHeaders(stream, good...); !errors.Is(err, rpc.ErrStreamContract) {
			t.Fatal("incomplete contract accepted", err)
		}
		want := 0
		if header == nil {
			want = 1
		}
		if stream.headers != 1 || stream.receives != want {
			t.Fatal("unexpected stream reads", stream)
		}
	}
	stream := &startupStream{header: metadata.Pairs("sample-capability", "v1", "sample-scope", "all-v1", "extra", "ignored")}
	if err := rpc.RequireStreamHeaders(stream, good...); err != nil || stream.headers != 1 || stream.receives != 0 {
		t.Fatal("valid headers consumed event", err)
	}
	failure, err := status.New(codes.Unavailable, "private message").WithDetails(wrapperspb.String("private detail"))
	if err != nil {
		t.Fatal(err)
	}
	original := failure.Err()
	for _, stream := range []*startupStream{{headerErr: original}, {receiveErr: original}} {
		if err := rpc.RequireStreamHeaders(stream, good...); err != original {
			t.Fatal("RPC error identity or details changed", err)
		}
	}
	// This invalid implementation sends a value before headers. It cannot satisfy
	// the contract, even if Recv reports success.
	if err := rpc.RequireStreamHeaders(&startupStream{}, good...); !errors.Is(err, rpc.ErrStreamContract) {
		t.Fatal(err)
	}
}

func BenchmarkStreamHeaderContract(b *testing.B) {
	for _, count := range []int{1, 2, 32} {
		name := "single"
		if count == 2 {
			name = "replay"
		} else if count == 32 {
			name = "maximum"
		}
		b.Run(name, func(b *testing.B) {
			stream := &startupStream{header: metadata.MD{}}
			var required []rpc.StreamHeader
			for i := range count {
				key := "capability-" + string(rune('a'+i/26)) + string(rune('a'+i%26))
				value := "v1"
				if count == 32 {
					key += strings.Repeat("a", 256-len(key))
					value = strings.Repeat("v", 1024)
				}
				stream.header.Set(key, value)
				required = append(required, rpc.StreamHeader{Name: key, Value: value})
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if err := rpc.RequireStreamHeaders(stream, required...); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
