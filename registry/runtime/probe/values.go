package probe

import (
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// TextLimit is the most bytes of one text value that the runtime sends (ST-SUP-RP12-LC09).
const TextLimit = 256

// valueKind is the kind of one typed value. The zero kind is no value.
type valueKind uint8

const (
	textValue valueKind = iota + 1
	integerValue
	boolValue
	enumValue
	measureValue
	numberValue
)

// Value is one typed value of an emission. Only the constructors of this package make one, so an emission carries
// typed values and never free text that a declaration does not declare (ST-SUP-RP12-LC01).
type Value struct {
	kind   valueKind
	text   string
	number int64
	flag   bool
	float  float64
}

// Text is the value of an entity field of type text, which the generated probe method reads from a loaded value.
func Text(value string) Value { return Value{kind: textValue, text: value} }

// Integer is the value of an entity field of an integer type, which the generated probe method reads from a loaded
// value.
func Integer(value int64) Value { return Value{kind: integerValue, number: value} }

// Bool is the value of a bool field, or of an entity field of type bool.
func Bool(value bool) Value { return Value{kind: boolValue, flag: value} }

// Enum is the value of an enum field: the declared text of one of its constants.
func Enum(value string) Value { return Value{kind: enumValue, text: value} }

// Measure is the value of a measure field, in the field's unit.
func Measure(value int64) Value { return Value{kind: measureValue, number: value} }

// Number is the value that a histogram records, in the histogram's unit. It is the last value of a histogram's
// emission.
func Number(value float64) Value { return Value{kind: numberValue, float: value} }

// matches tells whether v is a value of the field's type and inside its declared set or range.
func (f *field) matches(v Value) bool {
	switch f.typ {
	case FieldText:
		return v.kind == textValue
	case FieldInteger:
		return v.kind == integerValue
	case FieldBool:
		return v.kind == boolValue
	case FieldEnum:
		return v.kind == enumValue && f.values[v.text]
	case FieldMeasure:
		return v.kind == measureValue && v.number >= f.min && v.number <= f.max
	}
	return false
}

// finite tells whether v is a histogram value that can be recorded: a finite number that is not negative.
func (v Value) finite() bool {
	return v.kind == numberValue && !math.IsNaN(v.float) && !math.IsInf(v.float, 0) && v.float >= 0
}

// sent is the value as the runtime sends it: a text value cut to TextLimit bytes of valid UTF-8 with no NUL
// (ST-SUP-RP12-LC09).
func (v Value) sent() Value {
	if v.kind == textValue {
		v.text = cleanText(v.text)
	}
	return v
}

// String is the value as text, as the console sink and the recorder show it.
func (v Value) String() string {
	switch v.kind {
	case textValue, enumValue:
		return v.text
	case integerValue, measureValue:
		return strconv.FormatInt(v.number, 10)
	case boolValue:
		return strconv.FormatBool(v.flag)
	case numberValue:
		return strconv.FormatFloat(v.float, 'g', -1, 64)
	}
	return ""
}

// cleanText makes text valid UTF-8 with no NUL, then cuts it to at most TextLimit bytes on a rune boundary.
func cleanText(text string) string {
	if !utf8.ValidString(text) {
		text = strings.ToValidUTF8(text, "�")
	}
	if strings.IndexByte(text, 0) >= 0 {
		text = strings.ReplaceAll(text, "\x00", "�")
	}
	if len(text) <= TextLimit {
		return text
	}
	cut := TextLimit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}
