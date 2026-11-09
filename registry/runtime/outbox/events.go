package outbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"stego.dev/runtime/tenant"
)

// NoticeChannel is the PostgreSQL notification channel of the outbox notices.
const NoticeChannel = "stego_outbox_v1"

// The bounds of the event source (ST-SUP-RC18-OC22).
const (
	MaxSubscriptions   = 64
	SubscriptionBuffer = 64
	MaxNoticeBytes     = 4096
)

// Event is the hint of one committed message of the subscriber's tenant: its identity, its destination and its
// resource key, never its payload (ST-SUP-RP01-LC21). A subscriber reads the message again under row-level
// security in a transaction of its tenant before it acts, so a forged notice changes nothing.
type Event struct {
	ID          uuid.UUID
	Destination string
	ResourceKey string
}

// notice is the payload of one NOTIFY: the event and the tenant it is for.
type notice struct {
	ID          string `json:"id"`
	TenantID    string `json:"tenant_id"`
	Destination string `json:"destination"`
	ResourceKey string `json:"resource_key"`
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

// Listener is one dedicated database session that listens for notifications. The library takes database/sql only,
// and database/sql has no notification API, so the source takes this interface; the runtime gives its one adapter
// in the connector package stego.dev/runtime/pgconn, the one package that imports the driver (ST-SUP-RP01-LC20,
// ST-STD-RUNTIME-R06).
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
	bad           atomic.Uint64
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
	tenant  string
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

// Subscribe adds one subscription for the tenant t, with a bounded queue, at most MaxSubscriptions at one time
// (ST-SUP-RP01-LC21). It refuses the zero tenant. The subscription gets only the events of t, and ends when ctx
// ends, when it is closed, when it falls behind, or when the source fails.
func (s *Source) Subscribe(ctx context.Context, t tenant.Tenant) (Subscription, error) {
	if t.IsZero() {
		return nil, tenant.ErrZero
	}
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
	sub := &subscription{source: s, tenant: t.ID(), queue: make(chan Event, SubscriptionBuffer), done: make(chan struct{})}
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

// BadNotices counts the notices that could not be read and were skipped (ST-SUP-RP01-LC21).
func (s *Source) BadNotices() uint64 { return s.bad.Load() }

func (s *Source) dispatch(tenantID string, event Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for sub := range s.subscriptions {
		if sub.tenant != tenantID {
			continue
		}
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

// Run reads notifications until ctx ends or the session is lost, and passes each event to the subscriptions of its
// tenant. It is the supervised task of the source: a lost session returns ErrEventSourceUnavailable and stops every
// subscription (ST-SUP-RP01-LC05); a notice that cannot be read is counted and skipped and stops nothing
// (ST-SUP-RP01-LC21); the end of ctx returns nil. A source runs once.
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
		tenantID, event, err := decodeNotice([]byte(notification.Payload))
		if err != nil {
			s.bad.Add(1)
			continue
		}
		s.dispatch(tenantID, event)
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

func encodeNotice(tenantID string, event Event) (string, error) {
	data, err := json.Marshal(notice{ID: event.ID.String(), TenantID: tenantID, Destination: event.Destination,
		ResourceKey: event.ResourceKey})
	if err != nil {
		return "", err
	}
	if len(data) > MaxNoticeBytes {
		return "", errors.New("outbox: the notice is longer than its limit")
	}
	return string(data), nil
}

// decodeNotice reads one notice: a JSON object with exactly the four members of a notice, each once.
func decodeNotice(data []byte) (string, Event, error) {
	bad := errors.New("outbox: the notice cannot be read")
	if len(data) > MaxNoticeBytes || !utf8.Valid(data) {
		return "", Event{}, bad
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return "", Event{}, bad
	}
	values := map[string]string{}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return "", Event{}, bad
		}
		name, ok := key.(string)
		if !ok {
			return "", Event{}, bad
		}
		if _, exists := values[name]; exists {
			return "", Event{}, bad
		}
		switch name {
		case "id", "tenant_id", "destination", "resource_key":
		default:
			return "", Event{}, bad
		}
		var value string
		if err := decoder.Decode(&value); err != nil {
			return "", Event{}, bad
		}
		values[name] = value
	}
	if _, err := decoder.Token(); err != nil || len(values) != 4 {
		return "", Event{}, bad
	}
	if _, err := decoder.Token(); err != io.EOF {
		return "", Event{}, bad
	}
	id, err := uuid.Parse(values["id"])
	if err != nil || id == uuid.Nil || id.String() != values["id"] {
		return "", Event{}, bad
	}
	tenantID := values["tenant_id"]
	if tenantID == "" || !validText(tenantID, MaxKeyBytes) {
		return "", Event{}, bad
	}
	event := Event{ID: id, Destination: values["destination"], ResourceKey: values["resource_key"]}
	if err := validateMessage(Message{ID: event.ID, Destination: event.Destination, ResourceKey: event.ResourceKey,
		Kind: "notice", Payload: []byte(`{}`)}); err != nil {
		return "", Event{}, bad
	}
	return tenantID, event, nil
}
