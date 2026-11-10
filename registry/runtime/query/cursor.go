package query

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"

	"stego.dev/runtime/authz"
	"stego.dev/runtime/tenant"
)

// MinCursorKey is the fewest bytes of the key of the cursor MAC (ST-SUP-RP11-LC15).
const MinCursorKey = 32

// MaxCursorText is the longest text form of a cursor that ParseCursor reads.
const MaxCursorText = 4096

// ErrBadCursor is the error of a cursor text that was changed, that another query, tenant or principal got, or that
// does not decode. It names no part of the text.
var ErrBadCursor = errors.New("query: the cursor is not a cursor of this query")

// ErrNoCursorKey is the error of a cursor text with a runtime that has no cursor key.
var ErrNoCursorKey = errors.New("query: the runtime has no cursor key")

// The parts of the text form of a cursor.
const (
	cursorVersion = 1
	cursorTable   = 0
	cursorSearch  = 1

	consistencyNone    = 0
	consistencyFresh   = 1
	consistencyAtLeast = 2
)

// cursorDomain separates the MAC of a cursor from every other MAC that the key could make.
const cursorDomain = "stego.dev/runtime/query cursor v1"

// CursorText gives the text form of c for the next HTTP request of the query q, which the tenant t and the
// principal p run (ST-SUP-RP11-LC15). The text is opaque to the client: it holds the position and a keyed MAC
// (HMAC-SHA256 with the runtime's cursor key) over the position, the query name, the tenant and the principal, so
// ParseCursor refuses it when it is changed or used for another query, tenant or principal. The zero cursor, the
// start of a list, gives the empty text.
func (rt *Runtime) CursorText(q *Query, t tenant.Tenant, p authz.Principal, c Cursor) (string, error) {
	if c == (Cursor{}) {
		return "", nil
	}
	if len(rt.cursorKey) == 0 {
		return "", ErrNoCursorKey
	}
	if q == nil || t.IsZero() || p.IsZero() {
		return "", errors.New("query: a cursor needs its query, tenant and principal")
	}
	payload := []byte{cursorVersion}
	if c.page.IsZero() {
		payload = append(payload, cursorTable)
		payload = appendText(payload, c.after)
	} else {
		provider := c.page.Provider()
		cursor, first, _ := c.page.CursorFor(provider)
		payload = append(payload, cursorSearch)
		payload = appendText(payload, provider)
		payload = appendText(payload, cursor)
		token, atLeast := first.Token()
		switch {
		case atLeast:
			payload = append(payload, consistencyAtLeast)
			payload = appendText(payload, token.Text())
		case first.IsFresh():
			payload = append(payload, consistencyFresh)
		default:
			payload = append(payload, consistencyNone)
		}
	}
	mac := rt.cursorMAC(q, t, p, payload)
	return base64.RawURLEncoding.EncodeToString(append(payload, mac...)), nil
}

// ParseCursor reads the text form of a cursor of the query q for the tenant t and the principal p (ST-SUP-RP11-LC15).
// The empty text is the start of the list. It refuses, with ErrBadCursor and with no row read, a text that does not
// decode, whose MAC does not match because the text was changed, or that another query, tenant or principal got.
func (rt *Runtime) ParseCursor(q *Query, t tenant.Tenant, p authz.Principal, text string) (Cursor, error) {
	if text == "" {
		return Cursor{}, nil
	}
	if len(rt.cursorKey) == 0 {
		return Cursor{}, ErrNoCursorKey
	}
	if q == nil || t.IsZero() || p.IsZero() || len(text) > MaxCursorText {
		return Cursor{}, ErrBadCursor
	}
	raw, err := base64.RawURLEncoding.DecodeString(text)
	if err != nil || len(raw) < sha256.Size+2 {
		return Cursor{}, ErrBadCursor
	}
	payload, mac := raw[:len(raw)-sha256.Size], raw[len(raw)-sha256.Size:]
	if !hmac.Equal(mac, rt.cursorMAC(q, t, p, payload)) {
		return Cursor{}, ErrBadCursor
	}
	// The MAC matched, so the payload is one that CursorText wrote; it is still read with checks.
	if payload[0] != cursorVersion {
		return Cursor{}, ErrBadCursor
	}
	rest := payload[2:]
	switch payload[1] {
	case cursorTable:
		after, rest, ok := readText(rest)
		if !ok || len(rest) != 0 {
			return Cursor{}, ErrBadCursor
		}
		return Cursor{after: after, started: true}, nil
	case cursorSearch:
		provider, rest, ok1 := readText(rest)
		cursor, rest, ok2 := readText(rest)
		if !ok1 || !ok2 || len(rest) == 0 || cursor == "" {
			return Cursor{}, ErrBadCursor
		}
		var first authz.Consistency
		switch rest[0] {
		case consistencyAtLeast:
			text, tail, ok := readText(rest[1:])
			if !ok || len(tail) != 0 {
				return Cursor{}, ErrBadCursor
			}
			token, err := authz.ParseToken(text)
			if err != nil {
				return Cursor{}, ErrBadCursor
			}
			first = authz.AtLeast(token)
		case consistencyFresh:
			if len(rest) != 1 {
				return Cursor{}, ErrBadCursor
			}
			first = authz.Fresh()
		case consistencyNone:
			if len(rest) != 1 {
				return Cursor{}, ErrBadCursor
			}
		default:
			return Cursor{}, ErrBadCursor
		}
		return Cursor{page: authz.NewPageToken(provider, cursor, first)}, nil
	default:
		return Cursor{}, ErrBadCursor
	}
}

// cursorMAC is the HMAC-SHA256 of the payload of a cursor, bound to the query name, the tenant and the principal.
// Each part is length-prefixed, so no two sets of parts give the same input.
func (rt *Runtime) cursorMAC(q *Query, t tenant.Tenant, p authz.Principal, payload []byte) []byte {
	m := hmac.New(sha256.New, rt.cursorKey)
	var input []byte
	for _, part := range []string{cursorDomain, q.name, t.ID(), p.Subject().String()} {
		input = appendText(input, part)
	}
	input = append(input, payload...)
	m.Write(input)
	return m.Sum(nil)
}

// appendText appends text with its length as a uvarint.
func appendText(b []byte, text string) []byte {
	b = binary.AppendUvarint(b, uint64(len(text)))
	return append(b, text...)
}

// readText reads one text that appendText wrote, and the bytes after it.
func readText(b []byte) (string, []byte, bool) {
	n, size := binary.Uvarint(b)
	if size <= 0 || n > uint64(len(b)-size) {
		return "", nil, false
	}
	end := size + int(n)
	return string(b[size:end]), b[end:], true
}
