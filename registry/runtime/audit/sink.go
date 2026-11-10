package audit

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Sink is the audit-sink port (ST-SUP-RP05-LC08): an append-only store outside the service. It has no update, no
// delete and no read. The service reaches it only through the outbox destination (Destination), never from a
// handler.
type Sink interface {
	// Append stores the events. When an event ID is already stored, it returns an error that wraps ErrDuplicate.
	Append(ctx context.Context, events []Event) error
	// Anchor stores one anchor of one tenant's chain: its kind, head hash and count.
	Anchor(ctx context.Context, anchor Anchor) error
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
	v, err := decodeJSON("event", rec)
	if err != nil {
		return nil, err
	}
	return encodeTree(map[string]any{"kind": messageEvent, "event": v}), nil
}

func anchorMessage(a Anchor) []byte {
	return encodeTree(map[string]any{
		"kind":        messageAnchor,
		"tenant":      a.Tenant,
		"anchor_kind": string(a.Kind),
		"head":        hex.EncodeToString(a.Head),
		"count":       strconv.FormatInt(a.Count, 10),
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
	m, err := readSinkMessage(payload)
	if err != nil {
		return err
	}
	return d.send(ctx, m)
}

// ErrBadMessage matches the error of DeliverFor for a sink message that a retry cannot deliver: it does not decode,
// is not in canonical form, has a hash that does not cover its event, or names a tenant other than its outbox row's.
var ErrBadMessage = errors.New("audit: the sink message is refused")

// DeliverFor delivers one sink message of the outbox row of the tenant tenantID, as Deliver does, and first refuses,
// with an error that matches ErrBadMessage, a message that names another tenant or that Deliver cannot read
// (ST-SUP-RP05-LC14). An error of the sink does not match ErrBadMessage, so the outbox retries it.
//
// The outbox keeps the payload as the exact bytes that Record enqueued (ST-SUP-RP01-LC28), so an event message must
// still be in its canonical form, byte for byte, and its hash must cover its event.
func (d *Destination) DeliverFor(ctx context.Context, tenantID string, payload []byte) error {
	m, err := readSinkMessage(payload)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrBadMessage, err)
	}
	if tenantID == "" || m.tenantID != tenantID {
		return fmt.Errorf("%w: it names another tenant than its outbox row", ErrBadMessage)
	}
	return d.send(ctx, m)
}

// checkedMessage is one sink message that readSinkMessage checked: an event or an anchor.
type checkedMessage struct {
	event    *Event
	anchor   *Anchor
	tenantID string
}

// readSinkMessage reads and checks one sink message: an event message must be in canonical form and its hash must
// cover its event before the sink gets it.
func readSinkMessage(payload []byte) (checkedMessage, error) {
	v, err := decodeJSON("sink message", payload)
	if err != nil {
		return checkedMessage{}, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return checkedMessage{}, errors.New("audit: the sink message is not an object")
	}
	switch m["kind"] {
	case messageEvent:
		if !bytes.Equal(encodeTree(v), payload) {
			return checkedMessage{}, errors.New("audit: the sink message is not in canonical form")
		}
		e, err := eventFrom(m["event"])
		if err != nil {
			return checkedMessage{}, fmt.Errorf("audit: the sink message: %w", err)
		}
		h, err := Hash(e)
		if err != nil {
			return checkedMessage{}, err
		}
		if !bytes.Equal(h, e.Hash) {
			return checkedMessage{}, errors.New("audit: the hash of the sink message does not cover its event")
		}
		return checkedMessage{event: &e, tenantID: e.TenantID}, nil
	case messageAnchor:
		a, err := anchorFrom(m)
		if err != nil {
			return checkedMessage{}, fmt.Errorf("audit: the sink message: %w", err)
		}
		return checkedMessage{anchor: &a, tenantID: a.Tenant}, nil
	default:
		return checkedMessage{}, errors.New("audit: the sink message has an unknown kind")
	}
}

// send gives one checked message to the sink.
func (d *Destination) send(ctx context.Context, m checkedMessage) error {
	if m.anchor != nil {
		return d.sink.Anchor(ctx, *m.anchor)
	}
	err := d.sink.Append(ctx, []Event{*m.event})
	if errors.Is(err, ErrDuplicate) {
		return nil
	}
	return err
}

func anchorFrom(m map[string]any) (Anchor, error) {
	var a Anchor
	var err error
	if a.Tenant, err = text(m, "tenant"); err != nil {
		return a, err
	}
	kind, err := text(m, "anchor_kind")
	if err != nil {
		return a, err
	}
	a.Kind = AnchorKind(kind)
	if a.Head, err = hexField(m, "head"); err != nil {
		return a, err
	}
	c, err := text(m, "count")
	if err != nil {
		return a, err
	}
	if a.Count, err = strconv.ParseInt(c, 10, 64); err != nil {
		return a, errors.New("the field count is not a number")
	}
	return a, nil
}

// eventFrom reads an event from its canonical encoding (Encode). It holds the states that the redacting path
// wrote before the event was stored.
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
		if *t.dst, err = time.Parse(time.RFC3339Nano, val); err != nil {
			return Event{}, fmt.Errorf("the field %s is not a time", t.name)
		}
	}
	for _, s := range []struct {
		name string
		dst  *State
	}{{"before", &e.Before}, {"after", &e.After}, {"patch", &e.Patch}} {
		if val, present := m[s.name]; present {
			st, err := storedState(s.name, encodeTree(val))
			if err != nil {
				return Event{}, err
			}
			*s.dst = st
		}
	}
	// A message of hash_version 1 has neither operation nor hash_version (ST-SUP-RP05-LC07).
	e.HashVersion = HashVersion1
	if v, present := m["hash_version"]; present {
		n, ok := v.(json.Number)
		version, err := strconv.Atoi(string(n))
		if !ok || err != nil || (version != HashVersion2 && version != HashVersionCurrent) {
			return Event{}, errors.New("the field hash_version is not 2 or 3")
		}
		e.HashVersion = version
		if e.Operation, err = text(m, "operation"); err != nil {
			return Event{}, err
		}
		if version >= HashVersionCurrent {
			if e.OnBehalfOf, err = text(m, "on_behalf_of"); err != nil {
				return Event{}, err
			}
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
		return nil, fmt.Errorf("the field %s is not hex", name)
	}
	return b, nil
}
