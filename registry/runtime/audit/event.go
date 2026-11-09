package audit

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Event is one audit record (ST-SUP-RP05-LC01). It has a fixed shape: these 25 fields and no other. ActorIP and
// UserAgent come from the request and are Untrusted: the package only stores them and never decides on them.
type Event struct {
	ID                 UUID            `json:"id"`
	OccurredAt         time.Time       `json:"occurred_at"`
	RecordedAt         time.Time       `json:"recorded_at"`
	TenantID           string          `json:"tenant_id"`
	ActorID            string          `json:"actor_id"`
	ActorKind          ActorKind       `json:"actor_kind"`
	ActorIP            Untrusted       `json:"actor_ip"`
	UserAgent          Untrusted       `json:"user_agent"`
	Action             string          `json:"action"`
	ResourceType       string          `json:"resource_type"`
	ResourceID         string          `json:"resource_id"`
	Outcome            Outcome         `json:"outcome"`
	ErrorCode          string          `json:"error_code"`
	RequestID          string          `json:"request_id"`
	TraceID            string          `json:"trace_id"`
	SpanID             string          `json:"span_id"`
	IdempotencyKey     string          `json:"idempotency_key"`
	Before             json.RawMessage `json:"before,omitempty"`
	After              json.RawMessage `json:"after,omitempty"`
	Patch              json.RawMessage `json:"patch,omitempty"`
	Service            string          `json:"service"`
	ServiceVersion     string          `json:"service_version"`
	DeclarationVersion string          `json:"declaration_version"`
	PrevHash           []byte          `json:"prev_hash"`
	Hash               []byte          `json:"hash"`
}

// Untrusted is a value that the request supplies, such as the actor's address or user agent. The package stores
// it as it is and never uses it in a decision (ST-SUP-RP05-LC01).
type Untrusted string

// ActorKind is the kind of the actor of an event.
type ActorKind string

// The four actor kinds.
const (
	ActorUser           ActorKind = "user"
	ActorService        ActorKind = "service"
	ActorSystem         ActorKind = "system"
	ActorImpersonatedBy ActorKind = "impersonated_by"
)

// Outcome is the result of the audited action: ok, denied or failed.
type Outcome string

// The three outcomes.
const (
	OutcomeOK     Outcome = "ok"
	OutcomeDenied Outcome = "denied"
	OutcomeFailed Outcome = "failed"
)

// ActionRead is the action of the audit row of a read of an entity that holds a pii field (ST-SUP-RP05-LC02).
const ActionRead = "read"

// Fixed error codes that the runtime writes with the outcome failed.
const (
	// ErrorCodeConflict is the error code of a command that ends in a conflict.
	ErrorCodeConflict = "conflict"
	// ErrorCodeCommitOutcomeUnknown is the error code of a command whose COMMIT has an outcome that is not known.
	ErrorCodeCommitOutcomeUnknown = "commit_outcome_unknown"
)

// Sensitivity is the declared sensitivity of an entity field.
type Sensitivity string

// The sensitivities that change what the audit record holds.
const (
	Plain  Sensitivity = ""
	PII    Sensitivity = "pii"
	Secret Sensitivity = "secret"
)

// Fields maps the top-level field names of an entity to their declared sensitivity. A field that is not in the
// map is plain.
type Fields map[string]Sensitivity

// Level is the audit level of a service: change records the states, metadata records no state.
type Level string

// The audit levels. The default is change.
const (
	LevelChange   Level = "change"
	LevelMetadata Level = "metadata"
)

// PIIMode is the service's audit.pii setting (ST-DECL-EN10).
type PIIMode string

// The audit.pii settings. Keep keeps the field in the source record only: the audit record is redacted with
// either setting (ST-SUP-RP05-LC03).
const (
	PIIRedact PIIMode = "redact"
	PIIKeep   PIIMode = "keep"
)

// Settings are the service's audit settings.
type Settings struct {
	Level Level
	PII   PIIMode
}

// RedactionMarker replaces the value of every pii field in the audit record.
const RedactionMarker = "[redacted]"

// UUID is a 16-byte UUID.
type UUID [16]byte

// String returns the UUID in the canonical 8-4-4-4-12 hex form.
func (u UUID) String() string {
	var b [36]byte
	hex.Encode(b[0:8], u[0:4])
	b[8] = '-'
	hex.Encode(b[9:13], u[4:6])
	b[13] = '-'
	hex.Encode(b[14:18], u[6:8])
	b[18] = '-'
	hex.Encode(b[19:23], u[8:10])
	b[23] = '-'
	hex.Encode(b[24:36], u[10:16])
	return string(b[:])
}

// ParseUUID reads a UUID in the 8-4-4-4-12 hex form.
func ParseUUID(s string) (UUID, error) {
	var u UUID
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return u, fmt.Errorf("audit: %q is not a UUID", s)
	}
	raw := s[0:8] + s[9:13] + s[14:18] + s[19:23] + s[24:36]
	if _, err := hex.Decode(u[:], []byte(raw)); err != nil {
		return u, fmt.Errorf("audit: %q is not a UUID: %w", s, err)
	}
	return u, nil
}

// NewID returns a UUID version 7 (RFC 9562) for the time now: 48 bits of Unix milliseconds, then random bits.
func NewID(now time.Time) (UUID, error) {
	var u UUID
	if _, err := rand.Read(u[6:]); err != nil {
		return u, fmt.Errorf("audit: read random bits for an event ID: %w", err)
	}
	ms := uint64(now.UnixMilli())
	u[0] = byte(ms >> 40)
	u[1] = byte(ms >> 32)
	u[2] = byte(ms >> 24)
	u[3] = byte(ms >> 16)
	u[4] = byte(ms >> 8)
	u[5] = byte(ms)
	u[6] = 0x70 | (u[6] & 0x0f)
	u[8] = 0x80 | (u[8] & 0x3f)
	return u, nil
}

// New completes an event for the time now: it sets an ID (UUID version 7) and the occurred time when they are
// empty, keeps every time in UTC at microsecond precision, as the table stores it, and validates the event.
func New(e Event, now time.Time) (Event, error) {
	if e.ID == (UUID{}) {
		id, err := NewID(now)
		if err != nil {
			return Event{}, err
		}
		e.ID = id
	}
	if e.OccurredAt.IsZero() {
		e.OccurredAt = now
	}
	e.OccurredAt = storedTime(e.OccurredAt)
	if !e.RecordedAt.IsZero() {
		e.RecordedAt = storedTime(e.RecordedAt)
	}
	if err := e.Validate(); err != nil {
		return Event{}, err
	}
	return e, nil
}

func storedTime(t time.Time) time.Time {
	return t.UTC().Truncate(time.Microsecond)
}

// Validate refuses an event whose actor kind or outcome is not declared, or that names no action or service.
func (e Event) Validate() error {
	switch e.ActorKind {
	case ActorUser, ActorService, ActorSystem, ActorImpersonatedBy:
	default:
		return fmt.Errorf("audit: actor_kind %q is not one of user, service, system and impersonated_by", string(e.ActorKind))
	}
	switch e.Outcome {
	case OutcomeOK, OutcomeDenied, OutcomeFailed:
	default:
		return fmt.Errorf("audit: outcome %q is not one of ok, denied and failed", string(e.Outcome))
	}
	if e.Action == "" {
		return errors.New("audit: action is empty")
	}
	if e.Service == "" {
		return errors.New("audit: service is empty")
	}
	return nil
}

// SetStates sets Before, After and Patch from the entity states before and after the action. A nil state is a
// state that does not exist, such as the before of a create. At the metadata level the event carries no state.
// At the change level each state is redacted (Redact) whatever the audit.pii setting, and the patch is the RFC
// 6902 patch between the redacted states (Diff), so no pii value and no secret field reaches the event.
func (e *Event) SetStates(s Settings, f Fields, before, after map[string]any) error {
	if s.Level == LevelMetadata {
		e.Before, e.After, e.Patch = nil, nil, nil
		return nil
	}
	if s.Level != LevelChange && s.Level != "" {
		return fmt.Errorf("audit: level %q is not change or metadata", string(s.Level))
	}
	patch, err := Diff(before, after, f)
	if err != nil {
		return err
	}
	b, err := encodeState(before, f)
	if err != nil {
		return err
	}
	a, err := encodeState(after, f)
	if err != nil {
		return err
	}
	p, err := Canonical(patch.tree())
	if err != nil {
		return err
	}
	e.Before, e.After, e.Patch = b, a, p
	return nil
}

func encodeState(state map[string]any, f Fields) (json.RawMessage, error) {
	if state == nil {
		return nil, nil
	}
	r, err := Redact(state, f)
	if err != nil {
		return nil, err
	}
	return Canonical(r)
}

// Redact returns a copy of an entity state in canonical form with every pii field replaced by RedactionMarker and
// every secret field removed (ST-SUP-RP05-LC03).
func Redact(state map[string]any, f Fields) (map[string]any, error) {
	if state == nil {
		return nil, nil
	}
	n, err := normalize(state)
	if err != nil {
		return nil, err
	}
	m := n.(map[string]any)
	for k := range m {
		switch f[k] {
		case PII:
			m[k] = RedactionMarker
		case Secret:
			delete(m, k)
		}
	}
	return m, nil
}

// CanonicalRecord returns the canonical encoding of the event without prev_hash and hash: the bytes the hash
// covers (ST-SUP-RP05-LC04).
func CanonicalRecord(e Event) ([]byte, error) {
	m, err := recordTree(e)
	if err != nil {
		return nil, err
	}
	return encodeTree(m), nil
}

// Encode returns the canonical encoding of the whole event, prev_hash and hash included, as the sink receives it.
func Encode(e Event) ([]byte, error) {
	m, err := recordTree(e)
	if err != nil {
		return nil, err
	}
	m["prev_hash"] = hex.EncodeToString(e.PrevHash)
	m["hash"] = hex.EncodeToString(e.Hash)
	return encodeTree(m), nil
}

func recordTree(e Event) (map[string]any, error) {
	m := map[string]any{
		"id":                  e.ID.String(),
		"occurred_at":         timeText(e.OccurredAt),
		"recorded_at":         timeText(e.RecordedAt),
		"tenant_id":           e.TenantID,
		"actor_id":            e.ActorID,
		"actor_kind":          string(e.ActorKind),
		"actor_ip":            string(e.ActorIP),
		"user_agent":          string(e.UserAgent),
		"action":              e.Action,
		"resource_type":       e.ResourceType,
		"resource_id":         e.ResourceID,
		"outcome":             string(e.Outcome),
		"error_code":          e.ErrorCode,
		"request_id":          e.RequestID,
		"trace_id":            e.TraceID,
		"span_id":             e.SpanID,
		"idempotency_key":     e.IdempotencyKey,
		"service":             e.Service,
		"service_version":     e.ServiceVersion,
		"declaration_version": e.DeclarationVersion,
	}
	for name, raw := range map[string]json.RawMessage{"before": e.Before, "after": e.After, "patch": e.Patch} {
		if len(raw) == 0 {
			continue
		}
		v, err := decodeJSON(raw)
		if err != nil {
			return nil, fmt.Errorf("audit: %s is not JSON: %w", name, err)
		}
		m[name] = v
	}
	return m, nil
}

func timeText(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000000Z")
}

func decodeJSON(raw []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	if d.More() {
		return nil, errors.New("more than one JSON value")
	}
	return normalize(v)
}

// Canonical returns the canonical encoding of a value (ST-SUP-RP05-LC04): object keys sorted at every level, no
// insignificant whitespace, each number in its shortest exact decimal form (1.0 is 1) and each time in UTC with six
// fraction digits. It takes the values that encoding/json decodes to, Go integers and floats, and time.Time.
func Canonical(v any) ([]byte, error) {
	n, err := normalize(v)
	if err != nil {
		return nil, err
	}
	return encodeTree(n), nil
}

// normalize copies a value into the canonical tree: map[string]any, []any, string, bool, nil and json.Number in
// canonical form.
func normalize(v any) (any, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case bool:
		return x, nil
	case string:
		return x, nil
	case json.Number:
		return canonicalNumber(string(x))
	case float64:
		return canonicalNumber(strconv.FormatFloat(x, 'g', -1, 64))
	case float32:
		return canonicalNumber(strconv.FormatFloat(float64(x), 'g', -1, 32))
	case int:
		return json.Number(strconv.FormatInt(int64(x), 10)), nil
	case int8:
		return json.Number(strconv.FormatInt(int64(x), 10)), nil
	case int16:
		return json.Number(strconv.FormatInt(int64(x), 10)), nil
	case int32:
		return json.Number(strconv.FormatInt(int64(x), 10)), nil
	case int64:
		return json.Number(strconv.FormatInt(x, 10)), nil
	case uint:
		return json.Number(strconv.FormatUint(uint64(x), 10)), nil
	case uint8:
		return json.Number(strconv.FormatUint(uint64(x), 10)), nil
	case uint16:
		return json.Number(strconv.FormatUint(uint64(x), 10)), nil
	case uint32:
		return json.Number(strconv.FormatUint(uint64(x), 10)), nil
	case uint64:
		return json.Number(strconv.FormatUint(x, 10)), nil
	case time.Time:
		return timeText(x), nil
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, e := range x {
			n, err := normalize(e)
			if err != nil {
				return nil, err
			}
			m[k] = n
		}
		return m, nil
	case []any:
		a := make([]any, len(x))
		for i, e := range x {
			n, err := normalize(e)
			if err != nil {
				return nil, err
			}
			a[i] = n
		}
		return a, nil
	case []string:
		a := make([]any, len(x))
		for i, e := range x {
			a[i] = e
		}
		return a, nil
	case json.RawMessage:
		return decodeJSON(x)
	default:
		return nil, fmt.Errorf("audit: a value of type %T has no canonical encoding", v)
	}
}

// canonicalNumber writes a JSON number in its shortest exact decimal form, with no exponent.
func canonicalNumber(s string) (json.Number, error) {
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		exp, err := strconv.Atoi(s[i+1:])
		if err != nil || exp > 400 || exp < -400 {
			return "", fmt.Errorf("audit: the number %s is out of the canonical range", s)
		}
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return "", fmt.Errorf("audit: %q is not a number", s)
	}
	if r.IsInt() {
		return json.Number(r.Num().String()), nil
	}
	// The value is a finite decimal: its denominator divides a power of ten.
	digits := 0
	ten := big.NewInt(10)
	p := big.NewInt(1)
	rem := new(big.Int)
	for rem.Mod(p, r.Denom()).Sign() != 0 {
		p.Mul(p, ten)
		digits++
	}
	out := r.FloatString(digits)
	out = strings.TrimRight(out, "0")
	return json.Number(out), nil
}

func encodeTree(v any) []byte {
	var b bytes.Buffer
	writeTree(&b, v)
	return b.Bytes()
}

func writeTree(b *bytes.Buffer, v any) {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case string:
		writeString(b, x)
	case json.Number:
		b.WriteString(string(x))
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			writeString(b, k)
			b.WriteByte(':')
			writeTree(b, x[k])
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			writeTree(b, e)
		}
		b.WriteByte(']')
	}
}

const hexDigits = "0123456789abcdef"

func writeString(b *bytes.Buffer, s string) {
	b.WriteByte('"')
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r < 0x20:
			b.WriteString(`\u00`)
			b.WriteByte(hexDigits[r>>4])
			b.WriteByte(hexDigits[r&0xf])
		case r == utf8.RuneError && size == 1:
			b.WriteRune(utf8.RuneError)
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	b.WriteByte('"')
}
