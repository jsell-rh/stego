package query

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// FieldKind is the kind of value of a field that a list search compares: text, a whole number of 32 or 64 bits, a
// decimal number or a bool. The generated code gives the kind of each declared field that a search may name.
type FieldKind int

// The kinds of a field of a list search.
const (
	KindText FieldKind = iota + 1
	KindInt32
	KindInt64
	KindFloat
	KindBool
)

// MaxSearch is the longest text of a list search.
const MaxSearch = 4096

// ParseSearch reads the search of a list into its filter (ST-SUP-RP11-LC20): one or more conditions
// field = value joined by and, in any case of and, where field is one of fields and value is a text in single
// quotes, with two quotes for one and no NUL, a whole number in the range of its kind, a decimal number, or true or
// false, of the field's kind. It refuses, with an error that wraps ErrBadRequest and holds no part of the text, a
// search longer than MaxSearch, that is not valid UTF-8, that has more than most conditions, that names another
// field or a field twice, or that has any other form. most is the room that the list's own conditions, such as a
// scope and implicit fields, leave of MaxFilter. The empty search is no filter.
func ParseSearch(text string, fields map[string]FieldKind, most int) ([]Equal, error) {
	bad := func(what string) error { return fmt.Errorf("%w: the search %s", ErrBadRequest, what) }
	if len(text) > MaxSearch || !utf8.ValidString(text) {
		return nil, bad("is longer than 4096 bytes or not valid UTF-8")
	}
	p := searchParser{text: text}
	p.space()
	if p.done() {
		return nil, nil
	}
	var where []Equal
	seen := map[string]bool{}
	for {
		name := p.word()
		kind, ok := fields[name]
		if !ok || seen[name] {
			return nil, bad("names a field that a search does not take, or names one twice")
		}
		seen[name] = true
		p.space()
		if !p.take("=") {
			return nil, bad("has a condition that is not field = value")
		}
		p.space()
		value, ok := p.value(kind)
		if !ok {
			return nil, bad("has a value that is not of its field's kind")
		}
		where = append(where, Equal{Field: name, Value: value})
		if len(where) > min(most, MaxFilter) {
			return nil, bad(fmt.Sprintf("has more than %d conditions", min(most, MaxFilter)))
		}
		p.space()
		if p.done() {
			return where, nil
		}
		if !strings.EqualFold(p.word(), "and") {
			return nil, bad("joins its conditions with another word than and")
		}
		p.space()
	}
}

// searchParser reads the text of a search from its position.
type searchParser struct {
	text string
	at   int
}

func (p *searchParser) done() bool { return p.at >= len(p.text) }

func (p *searchParser) space() {
	for !p.done() && (p.text[p.at] == ' ' || p.text[p.at] == '\t') {
		p.at++
	}
}

func (p *searchParser) take(s string) bool {
	if strings.HasPrefix(p.text[p.at:], s) {
		p.at += len(s)
		return true
	}
	return false
}

// word reads a name of lower-case letters, digits and underscores, or a word such as and.
func (p *searchParser) word() string {
	start := p.at
	for !p.done() {
		c := p.text[p.at]
		if c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			p.at++
			continue
		}
		break
	}
	return p.text[start:p.at]
}

// value reads one value of the kind.
func (p *searchParser) value(kind FieldKind) (any, bool) {
	switch kind {
	case KindText:
		if !p.take("'") {
			return nil, false
		}
		var b strings.Builder
		for !p.done() {
			c := p.text[p.at]
			p.at++
			if c != '\'' {
				b.WriteByte(c)
				continue
			}
			if p.take("'") {
				b.WriteByte('\'')
				continue
			}
			if strings.ContainsRune(b.String(), 0) {
				return nil, false
			}
			return b.String(), true
		}
		return nil, false
	case KindBool:
		switch strings.ToLower(p.word()) {
		case "true":
			return true, true
		case "false":
			return false, true
		}
		return nil, false
	case KindInt32, KindInt64, KindFloat:
		start := p.at
		for !p.done() && strings.IndexByte("+-.0123456789eE", p.text[p.at]) >= 0 {
			p.at++
		}
		number := p.text[start:p.at]
		switch kind {
		case KindInt32:
			n, err := strconv.ParseInt(number, 10, 32)
			return int32(n), err == nil
		case KindInt64:
			n, err := strconv.ParseInt(number, 10, 64)
			return n, err == nil
		}
		f, err := strconv.ParseFloat(number, 64)
		return f, err == nil && !isInfOrNaN(f)
	}
	return nil, false
}

func isInfOrNaN(f float64) bool {
	return f != f || f > 1.7976931348623157e308 || f < -1.7976931348623157e308
}
