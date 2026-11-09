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
// UserAgent come from the request and are Untrusted: the package cleans and stores them and never decides on them.
// Before, After and Patch are of the type State, which holds its bytes unexported: only SetStates, the redacting
// path, makes a State that is not empty (ST-SUP-RP05-LC13).
type Event struct {
	ID                 UUID      `json:"id"`
	OccurredAt         time.Time `json:"occurred_at"`
	RecordedAt         time.Time `json:"recorded_at"`
	TenantID           string    `json:"tenant_id"`
	ActorID            string    `json:"actor_id"`
	ActorKind          ActorKind `json:"actor_kind"`
	ActorIP            Untrusted `json:"actor_ip"`
	UserAgent          Untrusted `json:"user_agent"`
	Action             string    `json:"action"`
	ResourceType       string    `json:"resource_type"`
	ResourceID         string    `json:"resource_id"`
	Outcome            Outcome   `json:"outcome"`
	ErrorCode          string    `json:"error_code"`
	RequestID          string    `json:"request_id"`
	TraceID            string    `json:"trace_id"`
	SpanID             string    `json:"span_id"`
	IdempotencyKey     string    `json:"idempotency_key"`
	Before             State     `json:"before,omitzero"`
	After              State     `json:"after,omitzero"`
	Patch              State     `json:"patch,omitzero"`
	Service            string    `json:"service"`
	ServiceVersion     string    `json:"service_version"`
	DeclarationVersion string    `json:"declaration_version"`
	PrevHash           []byte    `json:"prev_hash"`
	Hash               []byte    `json:"hash"`
}

// State is a redacted entity state or patch in canonical JSON. Its bytes are not exported, so no caller can put a
// raw value in an event: SetStates makes it from the redacting path, and the package reads it back from stored
// rows only.
type State struct {
	raw []byte
}

// JSON returns a copy of the canonical JSON, or nil for an empty State.
func (s State) JSON() json.RawMessage { return bytes.Clone(s.raw) }

// IsZero reports an empty State, which the encoding leaves out.
func (s State) IsZero() bool { return len(s.raw) == 0 }

// MarshalJSON returns the canonical JSON, or null for an empty State.
func (s State) MarshalJSON() ([]byte, error) {
	if len(s.raw) == 0 {
		return []byte("null"), nil
	}
	return bytes.Clone(s.raw), nil
}

// Untrusted is a value that the request supplies, such as the actor's address or user agent. New cleans it; the
// package never uses it in a decision (ST-SUP-RP05-LC01, ST-SUP-RP05-LC13).
type Untrusted string

// MaxUntrusted is the most bytes that an untrusted value keeps after New cleans it.
const MaxUntrusted = 512

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

// Fields maps the path of each sensitive field of an entity to its sensitivity. A path is the member names from
// the top of the state, joined by dots, such as email or profile.email; array elements take the path of their
// array, so items.card applies to the member card of each element of items. A field that is not in the map is
// plain.
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

// check refuses an unknown level or audit.pii setting. Both known settings redact the audit record: keep binds
// the source record only.
func (s Settings) check() error {
	switch s.Level {
	case "", LevelChange, LevelMetadata:
	default:
		return errors.New("audit: the level is not change or metadata")
	}
	switch s.PII {
	case "", PIIRedact, PIIKeep:
		return nil
	default:
		return errors.New("audit: the audit.pii setting is not redact or keep")
	}
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
		return u, errors.New("audit: the text is not a UUID")
	}
	raw := s[0:8] + s[9:13] + s[14:18] + s[19:23] + s[24:36]
	if _, err := hex.Decode(u[:], []byte(raw)); err != nil {
		return u, errors.New("audit: the text is not a UUID")
	}
	return u, nil
}

// NewID returns a UUID version 7 (RFC 9562) for the time now: 48 bits of Unix milliseconds, then random bits.
func NewID(now time.Time) (UUID, error) {
	var u UUID
	if _, err := rand.Read(u[6:]); err != nil {
		return u, errors.New("audit: no random bits for an event ID")
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

// New completes an event for the time now (ST-SUP-RP05-LC01, ST-SUP-RP05-LC13): it sets an ID (UUID version 7)
// and the occurred time when they are empty, keeps each time in UTC at microsecond precision, as the table stores
// it, cleans ActorIP and UserAgent, and the request values RequestID and IdempotencyKey, and validates the event.
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
	e.ActorIP = Untrusted(clean(string(e.ActorIP)))
	e.UserAgent = Untrusted(clean(string(e.UserAgent)))
	// The request ID and the idempotency key also come from the request, so they are cleaned the same way.
	e.RequestID = clean(e.RequestID)
	e.IdempotencyKey = clean(e.IdempotencyKey)
	if err := e.Validate(); err != nil {
		return Event{}, err
	}
	return e, nil
}

// clean is the cleaning of an untrusted value (ST-SUP-RP05-LC13): each invalid UTF-8 byte becomes U+FFFD, each
// NUL goes, and the value is cut to at most MaxUntrusted bytes at a rune boundary. It reads the bytes only to clean
// them; it decides nothing about the audited action.
func clean(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		piece := string(r)
		if r == utf8.RuneError && size == 1 {
			piece = string(utf8.RuneError)
		}
		if r == 0 {
			continue
		}
		if b.Len()+len(piece) > MaxUntrusted {
			break
		}
		b.WriteString(piece)
	}
	return b.String()
}

func storedTime(t time.Time) time.Time {
	return t.UTC().Truncate(time.Microsecond)
}

// Validate refuses an event whose actor kind or outcome is not declared, or that names no action, service or
// tenant. Its errors name the field and never the value.
func (e Event) Validate() error {
	switch e.ActorKind {
	case ActorUser, ActorService, ActorSystem, ActorImpersonatedBy:
	default:
		return errors.New("audit: actor_kind is not one of user, service, system and impersonated_by")
	}
	switch e.Outcome {
	case OutcomeOK, OutcomeDenied, OutcomeFailed:
	default:
		return errors.New("audit: outcome is not one of ok, denied and failed")
	}
	switch {
	case e.Action == "":
		return errors.New("audit: action is empty")
	case e.Service == "":
		return errors.New("audit: service is empty")
	case e.TenantID == "":
		return errors.New("audit: tenant_id is empty")
	}
	return nil
}

// SetStates is the redacting path (ST-SUP-RP05-LC03, ST-SUP-RP05-LC13), the only way to set the states of an
// event. A nil state is a state that does not exist, such as the before of a create. At the metadata level the
// event carries no state. At the change level each state is redacted (Redact) whatever the audit.pii setting, and
// the patch is the RFC 6902 patch between the redacted states (Diff), so no pii value and no secret field reaches
// the event.
func (e *Event) SetStates(s Settings, f Fields, before, after map[string]any) error {
	if err := s.check(); err != nil {
		return err
	}
	if s.Level == LevelMetadata {
		e.Before, e.After, e.Patch = State{}, State{}, State{}
		return nil
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
	e.Before, e.After, e.Patch = State{b}, State{a}, State{encodeTree(patch.tree())}
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
	return encodeTree(r), nil
}

// Redact returns a copy of an entity state in canonical form with every pii field, at any depth, replaced by
// RedactionMarker and every secret field, at any depth, removed before it is encoded (ST-SUP-RP05-LC03). A pii
// value is still checked to be encodable, so a change of it shows in the patch; an error names the field path and
// never a value.
func Redact(state map[string]any, f Fields) (map[string]any, error) {
	if state == nil {
		return nil, nil
	}
	r, err := redactAt("", "", state, f, nil)
	if err != nil {
		return nil, err
	}
	return r.(map[string]any), nil
}

// redactAt copies v. field is the dotted field path of v and pointer its JSON pointer; pii, when it is not nil,
// collects the canonical encoding of each pii value by its JSON pointer.
func redactAt(field, pointer string, v any, f Fields, pii map[string][]byte) (any, error) {
	switch x := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, e := range x {
			child := k
			if field != "" {
				child = field + "." + k
			}
			cp := pointer + "/" + escapePointer(k)
			switch f[child] {
			case Secret:
				continue
			case PII:
				n, err := normalize(child, e)
				if err != nil {
					return nil, err
				}
				if pii != nil {
					pii[cp] = encodeTree(n)
				}
				m[k] = RedactionMarker
			default:
				r, err := redactAt(child, cp, e, f, pii)
				if err != nil {
					return nil, err
				}
				m[k] = r
			}
		}
		return m, nil
	case []any:
		a := make([]any, len(x))
		for i, e := range x {
			r, err := redactAt(field, pointer+"/"+strconv.Itoa(i), e, f, pii)
			if err != nil {
				return nil, err
			}
			a[i] = r
		}
		return a, nil
	default:
		name := field
		if name == "" {
			name = "state"
		}
		return normalize(name, v)
	}
}

// storedState makes the State of a stored record, a row of the audit table or a sink message, from its JSON: the
// redacting path wrote it before it was stored, and the hash of the record covers it. It keeps the canonical form,
// so the hash comes out the same.
func storedState(name string, raw []byte) (State, error) {
	if len(raw) == 0 {
		return State{}, nil
	}
	v, err := decodeJSON(name, raw)
	if err != nil {
		return State{}, err
	}
	return State{encodeTree(v)}, nil
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
	for _, s := range []struct {
		name string
		raw  json.RawMessage
	}{{"before", e.Before.raw}, {"after", e.After.raw}, {"patch", e.Patch.raw}} {
		if len(s.raw) == 0 {
			continue
		}
		v, err := decodeJSON(s.name, s.raw)
		if err != nil {
			return nil, err
		}
		m[s.name] = v
	}
	return m, nil
}

func timeText(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000000Z")
}

// decodeJSON reads one JSON value; name names it in an error, which holds no value.
func decodeJSON(name string, raw []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil || d.More() {
		return nil, fmt.Errorf("audit: %s is not one JSON value", name)
	}
	return normalize(name, v)
}

// Canonical returns the canonical encoding of a value (ST-SUP-RP05-LC04): object keys sorted at every level, no
// insignificant whitespace, each number in its shortest exact decimal form (1.0 is 1) and each time in UTC with six
// fraction digits. It takes the values that encoding/json decodes to, Go integers and floats, and time.Time.
func Canonical(v any) ([]byte, error) {
	n, err := normalize("value", v)
	if err != nil {
		return nil, err
	}
	return encodeTree(n), nil
}

// normalize copies a value into the canonical tree: map[string]any, []any, string, bool, nil and json.Number in
// canonical form. field names the value in an error, which never holds the value.
func normalize(field string, v any) (any, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case bool:
		return x, nil
	case string:
		return x, nil
	case json.Number:
		return canonicalNumber(field, string(x))
	case float64:
		return canonicalNumber(field, strconv.FormatFloat(x, 'g', -1, 64))
	case float32:
		return canonicalNumber(field, strconv.FormatFloat(float64(x), 'g', -1, 32))
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
			n, err := normalize(field+"."+k, e)
			if err != nil {
				return nil, err
			}
			m[k] = n
		}
		return m, nil
	case []any:
		a := make([]any, len(x))
		for i, e := range x {
			n, err := normalize(field, e)
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
	default:
		return nil, fmt.Errorf("audit: the field %s has a type with no canonical encoding", field)
	}
}

// canonicalNumber writes a JSON number in its shortest exact decimal form, with no exponent. Its error names the
// field and not the number.
func canonicalNumber(field, s string) (json.Number, error) {
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		exp, err := strconv.Atoi(s[i+1:])
		if err != nil || exp > 400 || exp < -400 {
			return "", fmt.Errorf("audit: the number of the field %s is out of the canonical range", field)
		}
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return "", fmt.Errorf("audit: the field %s holds a number that is not a decimal", field)
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
	return json.Number(strings.TrimRight(r.FloatString(digits), "0")), nil
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
