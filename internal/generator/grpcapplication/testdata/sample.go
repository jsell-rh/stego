package sample

import (
	"context"
	"errors"
	events "example.com/grpc-test/out/contracts/events"
	storage "example.com/grpc-test/out/contracts/storage"
	pb "example.com/grpc-test/out/grpcapi/pb/sample/v1"
	transport "example.com/grpc-test/out/grpcapi/transport"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"strings"
	"sync/atomic"
	"time"
)

type Repository interface {
	storage.Storage
	storage.Transactor
	storage.ResourceLocker
}

var preparations atomic.Int32
var activeWaits atomic.Int32
var unavailableCalls atomic.Int32

type identityKey struct{}
type records struct{ pb.UnimplementedRecordsServer }

func Register(registrar grpc.ServiceRegistrar, _ Repository, _ ...events.Source) error {
	prepared, err := transport.PrepareRegistrar(registrar, func(ctx context.Context) error {
		if ctx.Value(identityKey{}) != "alice" {
			return errors.New("preparation lost verified identity")
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > transport.RequestTimeout {
			return errors.New("preparation has no deadline")
		}
		preparations.Add(1)
		if values := metadata.ValueFromIncomingContext(ctx, "x-test-prepare"); len(values) > 0 {
			if values[0] == "fail" {
				return status.Error(codes.Unavailable, "preparation failed")
			}
			if values[0] == "private" {
				return errors.New("private preparation details")
			}
			if values[0] == "wait" {
				<-ctx.Done()
				return ctx.Err()
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	pb.RegisterRecordsServer(prepared, records{})
	return nil
}
func (records) Echo(ctx context.Context, request *pb.Request) (*pb.Response, error) {
	if ctx.Value(identityKey{}) != "alice" {
		return nil, errors.New("identity was lost")
	}
	switch request.Text {
	case "private":
		return nil, errors.New("private-database-error")
	case "panic":
		panic("private-panic")
	case "unavailable":
		unavailableCalls.Add(1)
		return nil, status.Error(codes.Unavailable, "test failure")
	case "wait":
		activeWaits.Add(1)
		defer activeWaits.Add(-1)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return &pb.Response{Text: request.Text}, nil
}

var activeStreams atomic.Int32

func (records) Watch(request *pb.Request, stream grpc.ServerStreamingServer[pb.Response]) error {
	activeStreams.Add(1)
	defer activeStreams.Add(-1)
	if stream.Context().Value(identityKey{}) != "alice" {
		return errors.New("identity was lost")
	}
	if request.Text == "silent" {
		<-stream.Context().Done()
		return stream.Context().Err()
	}
	if request.Text == "hold" {
		if err := stream.SendHeader(nil); err != nil {
			return err
		}
		<-stream.Context().Done()
		return stream.Context().Err()
	}
	if request.Text == "flood" {
		if err := stream.SendHeader(nil); err != nil {
			return err
		}
		response := &pb.Response{Text: strings.Repeat("x", 1<<20)}
		for {
			if err := stream.Send(response); err != nil {
				return err
			}
		}
	}
	return stream.Send(&pb.Response{Text: "event"})
}
