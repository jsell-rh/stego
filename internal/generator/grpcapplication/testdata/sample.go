package sample

import (
	"context"
	"errors"
	storage "example.com/grpc-test/out/contracts/storage"
	pb "example.com/grpc-test/out/grpcapi/pb/sample/v1"
	"google.golang.org/grpc"
)

type Repository interface {
	storage.Storage
	storage.Transactor
}
type identityKey struct{}
type records struct{ pb.UnimplementedRecordsServer }

func Register(registrar grpc.ServiceRegistrar, _ Repository) error {
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
	case "wait":
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return &pb.Response{Text: request.Text}, nil
}
func (records) Watch(_ *pb.Request, stream grpc.ServerStreamingServer[pb.Response]) error {
	if stream.Context().Value(identityKey{}) != "alice" {
		return errors.New("identity was lost")
	}
	return stream.Send(&pb.Response{Text: "event"})
}
