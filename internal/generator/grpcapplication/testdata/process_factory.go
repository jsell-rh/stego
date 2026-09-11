package factory

import (
	"context"
	"errors"
	pb "example.com/grpc-test/out/grpcapi/pb/sample/v1"
	process "example.com/grpc-test/out/grpcapi/process"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"os"
	"runtime"
	"time"
)

func Open(ctx context.Context) (process.Application, error) {
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > 10*time.Second {
		return nil, errors.New("private-process missing deadline")
	}
	switch os.Getenv("PROCESS_TEST_MODE") {
	case "open-error":
		return nil, errors.New("private-process open")
	case "open-panic":
		panic("private-process open")
	case "open-goexit":
		runtime.Goexit()
	}
	return &application{}, nil
}

type application struct{ pb.UnimplementedRecordsServer }

func (a *application) Register(r grpc.ServiceRegistrar) error {
	if os.Getenv("PROCESS_TEST_MODE") == "register-error" {
		return errors.New("private-process register")
	}
	pb.RegisterRecordsServer(r, a)
	return nil
}
func (a *application) Close() error {
	if err := os.WriteFile(os.Getenv("PROCESS_TEST_CLOSED"), []byte("closed"), 0600); err != nil {
		return err
	}
	switch os.Getenv("PROCESS_TEST_MODE") {
	case "close-error":
		return errors.New("private-process close")
	case "close-panic":
		panic("private-process close")
	case "close-goexit":
		runtime.Goexit()
	}
	return nil
}
func (a *application) Echo(ctx context.Context, r *pb.Request) (*pb.Response, error) {
	if process.IdentityFromContext(ctx).UserID != "alice" {
		return nil, status.Error(codes.PermissionDenied, "not allowed")
	}
	if r.Text == "private" {
		return nil, errors.New("private-process handler")
	}
	return &pb.Response{Text: r.Text}, nil
}
func (a *application) Watch(r *pb.Request, s grpc.ServerStreamingServer[pb.Response]) error {
	if process.IdentityFromContext(s.Context()).UserID != "alice" {
		return status.Error(codes.PermissionDenied, "not allowed")
	}
	if err := s.Send(&pb.Response{Text: r.Text}); err != nil {
		return err
	}
	<-s.Context().Done()
	return s.Context().Err()
}
