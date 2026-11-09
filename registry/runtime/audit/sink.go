package audit

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// Sink is the audit-sink port (ST-SUP-RP05-LC08): an append-only store outside the service. It has no update, no
// delete and no read. The service reaches it only through the outbox destination (Destination), never from a
// handler.
type Sink interface {
	// Append stores the events. When an event ID is already stored, it returns an error that wraps ErrDuplicate.
	Append(ctx context.Context, events []Event) error
	// Anchor stores the head and the row count of one service chain.
	Anchor(ctx context.Context, head Head, count int64) error
}

// ErrDuplicate is the error a Sink wraps when it already holds an event ID.
var ErrDuplicate = errors.New("audit: the sink already holds the event")

// SinkDestination is the name of the outbox destination of the audit sink. Register it as idempotent: the sink
// treats a duplicate event ID as success.
const SinkDestination = "audit-sink"

// The kinds of sink message.
const (
	messageEvent  = "event"
	messageAnchor = "anchor"
)

func eventMessage(e Event) ([]byte, error) {
	rec, err := Encode(e)
	if err != nil {
		return nil, err
	}
	v, err := decodeJSON(rec)
	if err != nil {
		return nil, err
	}
	return encodeTree(map[string]any{"kind": messageEvent, "event": v}), nil
}

func anchorMessage(a Anchor) []byte {
	return encodeTree(map[string]any{
		"kind":    messageAnchor,
		"service": a.Head.Service,
		"head":    hex.EncodeToString(a.Head.Hash),
		"count":   fmt.Sprint(a.Count),
	})
}

// Destination delivers the audit sink messages of the outbox to a Sink (ST-SUP-RP05-LC07).
type Destination struct {
	sink Sink
}

// NewDestination returns the outbox destination for the sink.
func NewDestination(s Sink) (*Destination, error) {
	if s == nil {
		return nil, errors.New("audit: a destination needs a sink")
	}
	return &Destination{sink: s}, nil
}

// Deliver delivers one outbox message to the sink: an event message as one Append, an anchor message as one
// Anchor. A duplicate event ID is a success, so a message sent again completes.
func (d *Destination) Deliver(ctx context.Context, key string, payload []byte) error {
	v, err := decodeJSON(payload)
	if err != nil {
		return fmt.Errorf("audit: the sink message %s is not JSON: %w", key, err)
	}
	m, ok := v.(map[string]any)
	if !ok {
		return fmt.Errorf("audit: the sink message %s is not an object", key)
	}
	switch m["kind"] {
	case messageEvent:
		e, err := eventFrom(m["event"])
		if err != nil {
			return fmt.Errorf("audit: the sink message %s: %w", key, err)
		}
		err = d.sink.Append(ctx, []Event{e})
		if errors.Is(err, ErrDuplicate) {
			return nil
		}
		return err
	case messageAnchor:
		a, err := anchorFrom(m)
		if err != nil {
			return fmt.Errorf("audit: the sink message %s: %w", key, err)
		}
		return d.sink.Anchor(ctx, a.Head, a.Count)
	default:
		return fmt.Errorf("audit: the sink message %s has an unknown kind", key)
	}
}

func anchorFrom(m map[string]any) (Anchor, error) {
	var a Anchor
	var err error
	if a.Head.Service, err = text(m, "service"); err != nil {
		return a, err
	}
	if a.Head.Hash, err = hexField(m, "head"); err != nil {
		return a, err
	}
	c, err := text(m, "count")
	if err != nil {
		return a, err
	}
	if _, err := fmt.Sscan(c, &a.Count); err != nil {
		return a, fmt.Errorf("count %q is not a number", c)
	}
	return a, nil
}

// eventFrom reads an event from its canonical encoding (Encode).
func eventFrom(v any) (Event, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return Event{}, errors.New("the event is not an object")
	}
	var e Event
	strs := []struct {
		name string
		dst  *string
	}{
		{"tenant_id", &e.TenantID}, {"actor_id", &e.ActorID}, {"action", &e.Action},
		{"resource_type", &e.ResourceType}, {"resource_id", &e.ResourceID}, {"error_code", &e.ErrorCode},
		{"request_id", &e.RequestID}, {"trace_id", &e.TraceID}, {"span_id", &e.SpanID},
		{"idempotency_key", &e.IdempotencyKey}, {"service", &e.Service}, {"service_version", &e.ServiceVersion},
		{"declaration_version", &e.DeclarationVersion},
	}
	for _, s := range strs {
		val, err := text(m, s.name)
		if err != nil {
			return Event{}, err
		}
		*s.dst = val
	}
	var names [5]string
	for i, name := range []string{"id", "actor_kind", "actor_ip", "user_agent", "outcome"} {
		val, err := text(m, name)
		if err != nil {
			return Event{}, err
		}
		names[i] = val
	}
	id, err := ParseUUID(names[0])
	if err != nil {
		return Event{}, err
	}
	e.ID = id
	e.ActorKind, e.ActorIP, e.UserAgent, e.Outcome = ActorKind(names[1]), Untrusted(names[2]), Untrusted(names[3]),
		Outcome(names[4])
	for _, t := range []struct {
		name string
		dst  *time.Time
	}{{"occurred_at", &e.OccurredAt}, {"recorded_at", &e.RecordedAt}} {
		val, err := text(m, t.name)
		if err != nil {
			return Event{}, err
		}
		*t.dst, err = time.Parse(time.RFC3339Nano, val)
		if err != nil {
			return Event{}, fmt.Errorf("%s: %w", t.name, err)
		}
	}
	for _, s := range []struct {
		name string
		dst  *[]byte
	}{{"before", (*[]byte)(&e.Before)}, {"after", (*[]byte)(&e.After)}, {"patch", (*[]byte)(&e.Patch)}} {
		val, present := m[s.name]
		if present {
			*s.dst = encodeTree(val)
		}
	}
	if e.PrevHash, err = hexField(m, "prev_hash"); err != nil {
		return Event{}, err
	}
	if e.Hash, err = hexField(m, "hash"); err != nil {
		return Event{}, err
	}
	return e, nil
}

func text(m map[string]any, name string) (string, error) {
	s, ok := m[name].(string)
	if !ok {
		return "", fmt.Errorf("the field %s is not a string", name)
	}
	return s, nil
}

func hexField(m map[string]any, name string) ([]byte, error) {
	s, err := text(m, name)
	if err != nil {
		return nil, err
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("the field %s is not hex: %w", name, err)
	}
	return b, nil
}
