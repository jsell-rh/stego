package sample

import (
	"context"
	"errors"
	events "example.com/grpc-test/out/contracts/events"
	storage "example.com/grpc-test/out/contracts/storage"
	pb "example.com/grpc-test/out/grpcapi/pb/sample/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"strings"
	"sync/atomic"
)

type Repository interface {
	storage.Storage
	storage.Transactor
	storage.ResourceLocker
}

var activeWaits atomic.Int32
var unavailableCalls atomic.Int32

type identityKey struct{}
type records struct{ pb.UnimplementedRecordsServer }

func Register(registrar grpc.ServiceRegistrar, _ Repository, _ ...events.Source) error {
	pb.RegisterRecordsServer(registrar, records{})
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
