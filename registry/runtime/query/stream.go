package query

import (
	"context"
	"errors"
	"sync/atomic"
)

// ErrStreamEnded is the error of Send after the stream ended or the fill returned (ST-SUP-RP11-LC13).
var ErrStreamEnded = errors.New("query: the stream ended")

// Stream is the stream query handle of a query served as a server stream: the query handle, and Send, which sends
// one loaded value of the stream's entity as one message.
type Stream[T, P any] struct {
	handle  *Handle[P]
	entity  *Entity[T]
	ctx     context.Context
	send    func(T) error
	dropped atomic.Uint64
}

// Handle returns the query handle of the stream, for the fill's reads.
func (s *Stream[T, P]) Handle() *Handle[P] { return s.handle }

// Dropped counts the messages of this stream that a deny dropped. As RunStream states, the count must never reach the
// client.
func (s *Stream[T, P]) Dropped() uint64 { return s.dropped.Load() }

// Send sends value as one message of the stream (ST-SUP-RP11-LC13). It takes only a value that this handle loaded,
// of the stream's entity, and decides the read permission that the entity declares on it with the principal as
// subject at the floor of ST-SUP-RP06-LC09 at the time of the send, which it reads through the bound READ ONLY
// transaction alone. It sends the message only on an allow; on a deny it drops and counts the message and returns
// nil. Once the stream ended or the fill returned, it returns ErrStreamEnded or ErrClosed and sends nothing.
func (s *Stream[T, P]) Send(ctx context.Context, value *Loaded[T]) error {
	c := s.handle.core
	if err := c.life.enter(); err != nil {
		return err
	}
	defer c.life.leave()
	if s.ctx.Err() != nil || ctx.Err() != nil {
		return ErrStreamEnded
	}
	origin, e, id, token := value.row()
	if origin != c || e != &s.entity.e {
		return errForeignValue
	}
	allowed, err := c.allowRead(ctx, e, id, token)
	if err != nil {
		return err
	}
	if !allowed {
		s.dropped.Add(1)
		return nil
	}
	return s.send(value.value)
}

// StreamFill is the fill of a stream query: it gets the decoded request and the stream query handle, and sends its
// messages with Send.
type StreamFill[Req, T, P any] func(ctx context.Context, req Req, s *Stream[T, P]) error

// RunStream runs one stream query as Run runs a query: one READ ONLY transaction for the whole stream, one call of
// the fill, and the handle invalid once the fill returns. send writes one message of the entity e to the client; the
// stream ends when ctx ends, such as at the stream's lifetime, or when the fill returns. It returns the number of
// messages that a deny dropped, for the service's own telemetry only: the count must never reach the client, in a
// message, a trailer, a header or a status, because it tells the client how many resources it may not see.
func RunStream[Req, T, P any](ctx context.Context, rt *Runtime, q *Query, b Bound[P], req Req, e *Entity[T], send func(T) error, fill StreamFill[Req, T, P]) (uint64, error) {
	if fill == nil || send == nil || e == nil {
		return 0, errors.New("query: a stream needs its fill, its send and its entity")
	}
	var stream *Stream[T, P]
	err := runFill(ctx, rt, q, b, func(ctx context.Context, h *Handle[P]) error {
		if !h.core.reads[&e.e] {
			return ErrNotRead
		}
		stream = &Stream[T, P]{handle: h, entity: e, ctx: ctx, send: send}
		return fill(ctx, req, stream)
	})
	var dropped uint64
	if stream != nil {
		dropped = stream.dropped.Load()
		rt.dropped.Add(dropped)
	}
	return dropped, err
}
