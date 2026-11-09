package idempotency

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Fingerprint returns the fingerprint of a request (ST-SUP-RP07-LC05): the lower-case hexadecimal SHA-256 over the
// method, the path and the canonical JSON form of body, each part prefixed by its length in bytes as a 4-byte
// big-endian unsigned integer. An empty body is the empty part. A body that is not one JSON value is an error, so
// two bodies that cannot be told apart never share a fingerprint by accident.
func Fingerprint(method, path string, body []byte) (string, error) {
	canonical, err := Canonical(body)
	if err != nil {
		return "", err
	}
	sum, err := sumParts([]byte(method), []byte(path), canonical)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(sum[:]), nil
}

// Derive returns the outgoing idempotency key of one effect of an inbound request (ST-SUP-RP07-LC06): the lower-case
// hexadecimal SHA-256 over the tenant ID, the principal ID, the inbound key, the destination name and the effect
// name, each part prefixed by its length in bytes as a 4-byte big-endian unsigned integer. The value is the same on
// every retry of one inbound request, and differs for another tenant, principal, destination or effect.
func Derive(tenant, principal, inboundKey, destination, effect string) string {
	sum, err := sumParts([]byte(tenant), []byte(principal), []byte(inboundKey), []byte(destination), []byte(effect))
	if err != nil {
		// Only a part longer than 4 GiB fails, and no ID, key or name is that long.
		panic(err)
	}
	return hex.EncodeToString(sum[:])
}

// sumParts returns the SHA-256 over the parts, each prefixed by its length as a 4-byte big-endian unsigned integer.
func sumParts(parts ...[]byte) ([sha256.Size]byte, error) {
	h := sha256.New()
	var size [4]byte
	for _, part := range parts {
		if uint64(len(part)) > math.MaxUint32 {
			return [sha256.Size]byte{}, fmt.Errorf("idempotency: a part of %d bytes is longer than a 4-byte length prefix allows", len(part))
		}
		binary.BigEndian.PutUint32(size[:], uint32(len(part)))
		h.Write(size[:])
		h.Write(part)
	}
	var sum [sha256.Size]byte
	h.Sum(sum[:0])
	return sum, nil
}

// Canonical returns the canonical JSON form of body (ST-SUP-RP07-LC05): object keys sorted by their bytes, no
// insignificant whitespace, one form for each number and one escape form for each string. Numbers keep their exact
// decimal value, so 1, 1.0 and 1e0 have the form 1, and no integer loses digits. When an object repeats a key, the
// last value counts, as encoding/json decodes it. An empty or all-blank body gives an empty result.
func Canonical(body []byte) ([]byte, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return []byte{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	v, err := readValue(dec)
	if err != nil {
		return nil, fmt.Errorf("idempotency: the body is not valid JSON: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("idempotency: the body holds more than one JSON value")
	}
	var out bytes.Buffer
	if err := writeValue(&out, v); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// object is a decoded JSON object; value is one of nil, bool, string, json.Number, []value or object.
type object map[string]any

// readValue reads one JSON value from dec.
func readValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			obj := object{}
			for dec.More() {
				keyTok, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, ok := keyTok.(string)
				if !ok {
					return nil, errors.New("an object key is not a string")
				}
				v, err := readValue(dec)
				if err != nil {
					return nil, err
				}
				obj[key] = v
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return obj, nil
		case '[':
			arr := []any{}
			for dec.More() {
				v, err := readValue(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return arr, nil
		default:
			return nil, fmt.Errorf("unexpected delimiter %q", t)
		}
	default:
		return tok, nil
	}
}

// writeValue writes v in canonical form.
func writeValue(out *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case nil:
		out.WriteString("null")
	case bool:
		if t {
			out.WriteString("true")
		} else {
			out.WriteString("false")
		}
	case string:
		writeString(out, t)
	case json.Number:
		n, err := canonicalNumber(string(t))
		if err != nil {
			return err
		}
		out.WriteString(n)
	case []any:
		out.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				out.WriteByte(',')
			}
			if err := writeValue(out, e); err != nil {
				return err
			}
		}
		out.WriteByte(']')
	case object:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				out.WriteByte(',')
			}
			writeString(out, k)
			out.WriteByte(':')
			if err := writeValue(out, t[k]); err != nil {
				return err
			}
		}
		out.WriteByte('}')
	default:
		return fmt.Errorf("idempotency: unexpected JSON token %T", v)
	}
	return nil
}

// writeString writes s as a JSON string: the quote, the backslash and the control characters are escaped, each in
// one fixed form, and every other character is written as its UTF-8 bytes.
func writeString(out *bytes.Buffer, s string) {
	const hexDigits = "0123456789abcdef"
	out.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			out.WriteString(`\"`)
		case r == '\\':
			out.WriteString(`\\`)
		case r == '\b':
			out.WriteString(`\b`)
		case r == '\f':
			out.WriteString(`\f`)
		case r == '\n':
			out.WriteString(`\n`)
		case r == '\r':
			out.WriteString(`\r`)
		case r == '\t':
			out.WriteString(`\t`)
		case r < 0x20:
			out.WriteString(`\u00`)
			out.WriteByte(hexDigits[r>>4])
			out.WriteByte(hexDigits[r&0xf])
		default:
			var buf [utf8.UTFMax]byte
			n := utf8.EncodeRune(buf[:], r)
			out.Write(buf[:n])
		}
	}
	out.WriteByte('"')
}

// canonicalNumber returns the one form of the JSON number literal n, which keeps its exact decimal value: the
// significant digits with no leading or trailing zero, as an integer when the value is an integer of at most 21
// digits, as a plain decimal when its point is at most 6 places before the first digit, and otherwise as d.ddde+x.
// Zero, negative zero included, is 0.
func canonicalNumber(n string) (string, error) {
	neg := strings.HasPrefix(n, "-")
	rest := strings.TrimPrefix(n, "-")
	mantissa, expPart, hasExp := strings.Cut(strings.ToLower(rest), "e")
	exp := int64(0)
	if hasExp {
		e, err := strconv.ParseInt(strings.TrimPrefix(expPart, "+"), 10, 32)
		if err != nil {
			return "", fmt.Errorf("idempotency: the number %s has an exponent out of range", n)
		}
		exp = e
	}
	intPart, fracPart, _ := strings.Cut(mantissa, ".")
	digits := intPart + fracPart
	exp -= int64(len(fracPart))
	digits = strings.TrimLeft(digits, "0")
	if digits == "" {
		return "0", nil
	}
	trimmed := strings.TrimRight(digits, "0")
	exp += int64(len(digits) - len(trimmed))
	digits = trimmed
	// The value is digits × 10^exp. point is the place of the decimal point, counted from the left of digits.
	point := int64(len(digits)) + exp
	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	switch {
	case exp >= 0 && point <= 21:
		b.WriteString(digits)
		b.WriteString(strings.Repeat("0", int(exp)))
	case exp < 0 && point > 0:
		b.WriteString(digits[:point])
		b.WriteByte('.')
		b.WriteString(digits[point:])
	case point <= 0 && point > -6:
		b.WriteString("0.")
		b.WriteString(strings.Repeat("0", int(-point)))
		b.WriteString(digits)
	default:
		b.WriteByte(digits[0])
		if len(digits) > 1 {
			b.WriteByte('.')
			b.WriteString(digits[1:])
		}
		e := point - 1
		b.WriteByte('e')
		if e >= 0 {
			b.WriteByte('+')
		}
		b.WriteString(strconv.FormatInt(e, 10))
	}
	return b.String(), nil
}
