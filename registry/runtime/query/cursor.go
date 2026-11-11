package query

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"

	"github.com/jsell-rh/stego/registry/runtime/authz"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
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
	cursorVersion = 3
	cursorTable   = 0
	cursorSearch  = 1
	cursorOrdered = 2

	consistencyNone    = 0
	consistencyFresh   = 1
	consistencyAtLeast = 2
)

// cursorDomain separates the key and the associated data of a cursor from every other use of the cursor key.
const cursorDomain = "github.com/jsell-rh/stego/registry/runtime/query cursor v2"

// CursorText gives the text form of c for the next HTTP request of the query q, which the tenant t and the
// principal p run (ST-SUP-RP11-LC15). The position is sealed with AES-256-GCM under a key that HKDF-SHA256 derives
// from the runtime's cursor key, with a random nonce, and with the service, the query name, the tenant and the
// principal as associated data. So the text shows no key, no provider cursor and no token, and ParseCursor refuses
// it when it is changed or used for another service, query, tenant or principal. The zero cursor, the start of a
// list, gives the empty text.
func (rt *Runtime) CursorText(q *Query, t tenant.Tenant, p authz.Principal, c Cursor) (string, error) {
	if c == (Cursor{}) {
		return "", nil
	}
	if rt.cursorAEAD == nil {
		return "", ErrNoCursorKey
	}
	if q == nil || t.IsZero() || p.IsZero() {
		return "", errors.New("query: a cursor needs its query, tenant and principal")
	}
	payload := []byte{cursorVersion}
	switch {
	case c.order != "":
		// A list in order holds the order, the position of each order field and the key (ST-SUP-RP11-LC18).
		payload = append(payload, cursorOrdered)
		payload = appendText(payload, c.order)
		payload = appendText(payload, c.values)
		payload = appendText(payload, c.after)
		payload = appendText(payload, c.filter)
	case c.page.IsZero():
		payload = append(payload, cursorTable)
		payload = appendText(payload, c.after)
		payload = appendText(payload, c.filter)
	default:
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
	nonce := make([]byte, rt.cursorAEAD.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := rt.cursorAEAD.Seal(nonce, nonce, payload, rt.cursorData(q, t, p))
	return base64.RawURLEncoding.EncodeToString(append([]byte{cursorVersion}, sealed...)), nil
}

// ParseCursor reads the text form of a cursor of the query q for the tenant t and the principal p (ST-SUP-RP11-LC15).
// The empty text is the start of the list. It refuses, with ErrBadCursor and with no row read, a text that does not
// decode, that does not open because it was changed, or that another service, query, tenant or principal got.
func (rt *Runtime) ParseCursor(q *Query, t tenant.Tenant, p authz.Principal, text string) (Cursor, error) {
	if text == "" {
		return Cursor{}, nil
	}
	if rt.cursorAEAD == nil {
		return Cursor{}, ErrNoCursorKey
	}
	if q == nil || t.IsZero() || p.IsZero() || len(text) > MaxCursorText {
		return Cursor{}, ErrBadCursor
	}
	raw, err := base64.RawURLEncoding.DecodeString(text)
	nonceSize := rt.cursorAEAD.NonceSize()
	if err != nil || len(raw) < 1+nonceSize+rt.cursorAEAD.Overhead() || raw[0] != cursorVersion {
		return Cursor{}, ErrBadCursor
	}
	nonce, sealed := raw[1:1+nonceSize], raw[1+nonceSize:]
	payload, err := rt.cursorAEAD.Open(nil, nonce, sealed, rt.cursorData(q, t, p))
	if err != nil || len(payload) < 2 || payload[0] != cursorVersion {
		return Cursor{}, ErrBadCursor
	}
	rest := payload[2:]
	switch payload[1] {
	case cursorTable:
		after, rest, ok1 := readText(rest)
		filter, rest, ok2 := readText(rest)
		if !ok1 || !ok2 || len(rest) != 0 {
			return Cursor{}, ErrBadCursor
		}
		return Cursor{after: after, started: true, filter: filter}, nil
	case cursorOrdered:
		order, rest, ok1 := readText(rest)
		values, rest, ok2 := readText(rest)
		after, rest, ok3 := readText(rest)
		filter, rest, ok4 := readText(rest)
		if !ok1 || !ok2 || !ok3 || !ok4 || len(rest) != 0 || order == "" {
			return Cursor{}, ErrBadCursor
		}
		return Cursor{after: after, started: true, order: order, values: values, filter: filter}, nil
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

// cursorData is the associated data of a sealed cursor: the service, the query name, the tenant and the principal,
// each length-prefixed, so no two sets of parts give the same data.
func (rt *Runtime) cursorData(q *Query, t tenant.Tenant, p authz.Principal) []byte {
	var data []byte
	for _, part := range []string{cursorDomain, rt.service, q.name, t.ID(), p.Subject().String()} {
		data = appendText(data, part)
	}
	return data
}

// newCursorAEAD derives the cursor key with HKDF-SHA256 from key and returns its AES-256-GCM.
func newCursorAEAD(key []byte) (cipher.AEAD, error) {
	derived, err := hkdf.Key(sha256.New, key, nil, cursorDomain, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(derived)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
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
