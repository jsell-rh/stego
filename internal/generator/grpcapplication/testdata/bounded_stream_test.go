package client

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type terminalStream struct {
	grpc.ClientStream
	headerErr, receiveErr error
}

func (s *terminalStream) Header() (metadata.MD, error) { return nil, s.headerErr }
func (s *terminalStream) RecvMsg(any) error            { return s.receiveErr }

// A transport can return a terminal result before its OnFinish callback runs.
// No callback runs in this fixture; the wrapper must complete the observation.
func TestBoundedStreamTerminalObservation(t *testing.T) {
	for _, test := range []struct {
		name             string
		header, deadline bool
		err              error
		want             codes.Code
	}{
		{name: "header deadline", header: true, deadline: true, want: codes.DeadlineExceeded},
		{name: "receive deadline", deadline: true, want: codes.DeadlineExceeded},
		{name: "header error", header: true, err: status.Error(codes.Unavailable, "private"), want: codes.Unavailable},
		{name: "receive error", err: status.Error(codes.Canceled, "private"), want: codes.Canceled},
		{name: "EOF", err: io.EOF, want: codes.OK},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(context.Canceled)
			if test.deadline {
				cancel(context.DeadlineExceeded)
			}
			source := &terminalStream{}
			if test.header {
				source.headerErr = test.err
			} else {
				source.receiveErr = test.err
			}
			timer := time.NewTimer(time.Hour)
			defer timer.Stop()
			calls := 0
			var observed error
			stream := &boundedStream{ClientStream: source, ctx: ctx, handshake: timer, finish: func(err error) { calls++; observed = err }}
			var err error
			if test.header {
				_, err = stream.Header()
			} else {
				err = stream.RecvMsg(nil)
			}
			observedCode := status.Code(observed)
			if errors.Is(observed, context.DeadlineExceeded) {
				observedCode = codes.DeadlineExceeded
			}
			if calls != 1 || observedCode != test.want {
				t.Fatal("terminal observation did not complete before return", calls)
			}
			if test.deadline {
				if status.Code(err) != codes.DeadlineExceeded {
					t.Fatal("deadline result changed")
				}
			} else if err != test.err {
				t.Fatal("terminal result changed")
			}
		})
	}
}
