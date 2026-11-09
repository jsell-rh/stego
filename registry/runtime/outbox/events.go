package outbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// NoticeChannel is the PostgreSQL notification channel of the outbox notices.
const NoticeChannel = "stego_outbox_v1"

// The bounds of the event source (ST-SUP-RC18-OC22).
const (
	MaxSubscriptions   = 64
	SubscriptionBuffer = 64
	MaxNoticeBytes     = 4096
)

// Event is the notice of one committed message. It holds the message's identity and labels, never its payload.
// TenantID names the tenant of the message, so a subscriber passes on only the events of its own tenant.
type Event struct {
	ID          uuid.UUID `json:"id"`
	TenantID    string    `json:"tenant_id"`
	Destination string    `json:"destination"`
	ResourceKey string    `json:"resource_key"`
	Kind        string    `json:"kind"`
}

// Subscription is one reader of the event source.
type Subscription interface {
	Next(ctx context.Context) (Event, error)
	Close()
}

// The errors of the event source.
var (
	ErrEventSourceUnavailable = errors.New("outbox: the event source is not available")
	ErrEventCapacity          = errors.New("outbox: the event source holds its most subscriptions")
	ErrSlowConsumer           = errors.New("outbox: the subscriber fell behind; subscribe and list again")
)

// Notification is one notification of the database session.
type Notification struct {
	Channel string
	Payload string
}

// Listener is one dedicated database session that listens for notifications. The library takes database/sql only
// and imports no driver (ST-STD-RUNTIME-R06), and database/sql has no notification API, so the service passes a
// Listener over its driver's own connection, such as a pgx.Conn, opened for the source alone and in no pool.
type Listener interface {
	// Listen subscribes the session to channel.
	Listen(ctx context.Context, channel string) error
	// WaitForNotification returns the next notification, or an error when ctx ends or the session is lost.
	WaitForNotification(ctx context.Context) (Notification, error)
	// Close ends the session.
	Close(ctx context.Context) error
}

// Source owns one listening session. Run and Close own its lifetime. A lost session closes every subscription and
// the source; there is no silent reconnect, and a new source needs a new session (ST-SUP-RC18-OC10).
type Source struct {
	listener      Listener
	mu            sync.Mutex
	subscriptions map[*subscription]struct{}
	closed        bool
	started       bool
	cancel        context.CancelFunc
	done          chan struct{}
	closeOnce     sync.Once
}

type subscription struct {
	source  *Source
	queue   chan Event
	done    chan struct{}
	failure error
	stop    func() bool
}

// NewSource listens on NoticeChannel through listener. LISTEN completes before any subscription can be made. On an
// error it closes the listener.
func NewSource(ctx context.Context, listener Listener) (*Source, error) {
	if ctx == nil || listener == nil {
		return nil, errors.New("outbox: the event source needs a context and a listener")
	}
	check, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	if err := listener.Listen(check, NoticeChannel); err != nil {
		closeCtx, closeCancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer closeCancel()
		_ = listener.Close(closeCtx)
		return nil, ErrEventSourceUnavailable
	}
	return &Source{listener: listener, subscriptions: map[*subscription]struct{}{}}, nil
}

// Subscribe adds one subscription with a bounded queue, at most MaxSubscriptions at one time. The subscription ends
// when ctx ends, when it is closed, when it falls behind, or when the source fails.
func (s *Source) Subscribe(ctx context.Context) (Subscription, error) {
	if ctx == nil {
		return nil, ErrEventSourceUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrEventSourceUnavailable
	}
	if len(s.subscriptions) >= MaxSubscriptions {
		return nil, ErrEventCapacity
	}
	sub := &subscription{source: s, queue: make(chan Event, SubscriptionBuffer), done: make(chan struct{})}
	s.subscriptions[sub] = struct{}{}
	sub.stop = context.AfterFunc(ctx, func() { s.remove(sub, ctx.Err()) })
	return sub, nil
}

func (s *Source) remove(sub *subscription, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeLocked(sub, err)
}

func (s *Source) removeLocked(sub *subscription, err error) {
	if sub.failure != nil {
		return
	}
	if err == nil {
		err = context.Canceled
	}
	sub.failure = err
	delete(s.subscriptions, sub)
	close(sub.done)
	if sub.stop != nil {
		sub.stop()
	}
}

// Close ends the subscription.
func (sub *subscription) Close() { sub.source.remove(sub, context.Canceled) }

// Next returns the next event, or the error that ended the subscription: the cancellation of its context,
// ErrSlowConsumer or ErrEventSourceUnavailable.
func (sub *subscription) Next(ctx context.Context) (Event, error) {
	if ctx == nil {
		return Event{}, ErrEventSourceUnavailable
	}
	if err := sub.failureNow(); err != nil {
		return Event{}, err
	}
	select {
	case <-ctx.Done():
		return Event{}, ctx.Err()
	case <-sub.done:
		return Event{}, sub.failureNow()
	case event := <-sub.queue:
		if err := sub.failureNow(); err != nil {
			return Event{}, err
		}
		if err := ctx.Err(); err != nil {
			return Event{}, err
		}
		return event, nil
	}
}

func (sub *subscription) failureNow() error {
	sub.source.mu.Lock()
	defer sub.source.mu.Unlock()
	return sub.failure
}

func (s *Source) dispatch(event Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for sub := range s.subscriptions {
		select {
		case sub.queue <- event:
		default:
			s.removeLocked(sub, ErrSlowConsumer)
		}
	}
}

func (s *Source) fail() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	for sub := range s.subscriptions {
		s.removeLocked(sub, ErrEventSourceUnavailable)
	}
}

// Run reads notifications until ctx ends or the session is lost, and passes each event to every subscription. It is
// the supervised task of the source: a lost session or a notice that is not valid returns
// ErrEventSourceUnavailable and stops every subscription (ST-SUP-RP01-LC05); the end of ctx returns nil. A source
// runs once.
func (s *Source) Run(ctx context.Context) error {
	if ctx == nil {
		return ErrEventSourceUnavailable
	}
	s.mu.Lock()
	if s.closed || s.started {
		s.mu.Unlock()
		return ErrEventSourceUnavailable
	}
	s.started = true
	ctx, s.cancel = context.WithCancel(ctx)
	s.done = make(chan struct{})
	done := s.done
	s.mu.Unlock()
	defer close(done)
	defer s.fail()
	defer s.cancel()
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.listener.Close(closeCtx)
	}()
	for {
		notification, err := s.listener.WaitForNotification(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return ErrEventSourceUnavailable
		}
		if notification.Channel != NoticeChannel {
			continue
		}
		event, err := decodeEvent([]byte(notification.Payload))
		if err != nil {
			return ErrEventSourceUnavailable
		}
		s.dispatch(event)
	}
}

// Close stops the source and every subscription, and ends the session.
func (s *Source) Close() {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		if s.cancel != nil {
			s.cancel()
		}
		done := s.done
		for sub := range s.subscriptions {
			s.removeLocked(sub, ErrEventSourceUnavailable)
		}
		s.mu.Unlock()
		if done != nil {
			<-done
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.listener.Close(ctx)
	})
}

func encodeEvent(event Event) (string, error) {
	data, err := json.Marshal(event)
	if err != nil {
		return "", err
	}
	if len(data) > MaxNoticeBytes {
		return "", errors.New("outbox: the notice is longer than its limit")
	}
	return string(data), nil
}

// decodeEvent reads one notice: a JSON object with exactly the five members of an Event, each once.
func decodeEvent(data []byte) (Event, error) {
	var event Event
	if len(data) > MaxNoticeBytes || !utf8.Valid(data) {
		return event, ErrEventSourceUnavailable
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return event, ErrEventSourceUnavailable
	}
	values := map[string]string{}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return event, ErrEventSourceUnavailable
		}
		name, ok := key.(string)
		if !ok {
			return event, ErrEventSourceUnavailable
		}
		if _, exists := values[name]; exists {
			return event, ErrEventSourceUnavailable
		}
		switch name {
		case "id", "tenant_id", "destination", "resource_key", "kind":
		default:
			return event, ErrEventSourceUnavailable
		}
		var value string
		if err := decoder.Decode(&value); err != nil {
			return event, ErrEventSourceUnavailable
		}
		values[name] = value
	}
	if _, err := decoder.Token(); err != nil || len(values) != 5 {
		return event, ErrEventSourceUnavailable
	}
	if _, err := decoder.Token(); err != io.EOF {
		return event, ErrEventSourceUnavailable
	}
	id, err := uuid.Parse(values["id"])
	if err != nil || id == uuid.Nil || id.String() != values["id"] {
		return event, ErrEventSourceUnavailable
	}
	event = Event{ID: id, TenantID: values["tenant_id"], Destination: values["destination"],
		ResourceKey: values["resource_key"], Kind: values["kind"]}
	if event.TenantID == "" || !validText(event.TenantID, MaxKeyBytes) {
		return Event{}, ErrEventSourceUnavailable
	}
	if err := validateMessage(Message{ID: event.ID, Destination: event.Destination, ResourceKey: event.ResourceKey,
		Kind: event.Kind, Payload: []byte(`{}`)}); err != nil {
		return Event{}, ErrEventSourceUnavailable
	}
	return event, nil
}
