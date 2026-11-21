package sealed

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
)

// Redacted is the text that a Secret and a Compared give for each fmt verb, for String and for slog.
const Redacted = "[secret]"

// ErrDecode is the error of a Secret or a Compared that a JSON decoder fills: a secret field is never decoded from
// JSON into its stored form, only given through SecretOf or ComparedOf.
var ErrDecode = errors.New("sealed: a secret value is not decoded from JSON")

// ErrCounted is the error of Stored for a stored value whose field has a counter of wrong attempts: such a value gives
// its stored hash only to the runtime that bound its counter (StoredBy), so a fill cannot check a value against the
// hash with no charge (security review of PR #451, L1).
var ErrCounted = errors.New("sealed: a counted compare-only value gives no stored hash")

// Secret is the value of a readable secret field: its text, or not set. Only Reveal gives its text; it gives [secret]
// for each fmt verb, for String and for slog, and encodes as JSON null (ST-SUP-RP16-LC04). The text is behind a
// pointer to a string, so fmt and slog, which reach a value through an unexported field without its methods, print an
// address and never the text, also for a verb that does not fit (security review of PR #441, M1). The text never
// changes after SecretOf, so a copy of a Secret may share it.
type Secret struct{ text *string }

// SecretOf gives the Secret of text; an empty text gives a Secret that is not set.
func SecretOf(text string) Secret {
	if text == "" {
		return Secret{}
	}
	return Secret{text: &text}
}

// Reveal gives the text of the secret, or empty when it is not set.
func (s Secret) Reveal() string {
	if s.text == nil {
		return ""
	}
	return *s.text
}

// IsSet tells whether the secret has a text.
func (s Secret) IsSet() bool { return s.text != nil }

// Equal tells, in constant time for texts of one length, whether text is the secret. A secret that is not set is
// equal to no text, so a fill can check a given value with no import of its own.
func (s Secret) Equal(text string) bool {
	return s.text != nil && text != "" && constantEqual(*s.text, text)
}

// String gives [secret].
func (s Secret) String() string { return Redacted }

// GoString gives [secret].
func (s Secret) GoString() string { return Redacted }

// Format writes [secret] for each verb.
func (s Secret) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(Redacted)) }

// LogValue gives [secret] to slog.
func (s Secret) LogValue() slog.Value { return slog.StringValue(Redacted) }

// MarshalJSON encodes the secret as null.
func (s Secret) MarshalJSON() ([]byte, error) { return []byte("null"), nil }

// MarshalText encodes the secret as [secret], for an encoder that takes text, such as a map key.
func (s Secret) MarshalText() ([]byte, error) { return []byte(Redacted), nil }

// UnmarshalJSON refuses each value.
func (s *Secret) UnmarshalJSON([]byte) error { return ErrDecode }

// Compared is the value of a compare-only secret field: a new text that a write hashes, or the stored hash that a read
// gave. It never gives a text: Matches checks a given value (ST-SUP-RP16-LC03, ST-SUP-RP16-LC04). Its state is behind
// pointers to strings, as the text of Secret is: text is a new value that the next write hashes, and hash the stored
// Argon2id string; at most one is set.
type Compared struct {
	text, hash *string
	// counter counts the checks of a stored value whose field has a counter of wrong attempts; nil counts nothing
	// (ST-SUP-RP10-LC54). key is the store key of the runtime that bound the counter.
	counter Counter
	key     *StoreKey
}

// StoreKey is the key of the runtime that binds a counter to a stored value (CountedByKey): only the holder of that key
// gets the stored hash of the counted value back, through StoredBy, to write it again. The command runtime keeps its
// key in a variable of its own package, which fill code cannot reach.
type StoreKey struct{ _ byte }

// NewStoreKey gives a new store key, which no other key equals.
func NewStoreKey() *StoreKey { return &StoreKey{} }

// Counter counts the checks of a stored compare-only value whose field declares a limit of wrong attempts
// (ST-DECL-INV104, ST-SUP-RP10-LC54). The command runtime binds it to each value that its handle reads.
type Counter interface {
	// Charge takes one check of the limit before the check runs, and commits it on its own: false at the limit, when
	// the charge cannot commit, and for a value that its handle may not count, and then no check runs.
	Charge() bool
	// Record records the result of one check that Charge allowed.
	Record(matched bool)
}

// dummy is the hash that a check at the limit verifies against, made once.
var dummy struct {
	once sync.Once
	hash string
}

// dummyHash gives the hash of a fixed text, which no secret has: a check at the limit runs one verify against it.
func dummyHash() string {
	dummy.once.Do(func() {
		h, err := Hash("stego: the limit of wrong attempts is reached")
		if err == nil {
			dummy.hash = h
		}
	})
	return dummy.hash
}

// CountedBy gives c with the counter k and no store key: each later Matches of a stored value asks k first and records
// its result in k, and no StoredBy gives the stored hash. A value that has a counter keeps its counter, and a nil
// counter changes nothing. The command runtime calls CountedByKey.
func CountedBy(c Compared, k Counter) Compared {
	if c.counter != nil || k == nil {
		return c
	}
	c.counter = k
	return c
}

// CountedByKey gives c with the counter k and the store key key: each later Matches of a stored value asks k first
// and records its result in k, and only StoredBy with key gives the stored hash. The command runtime calls it for the
// value of a counted field that its handle reads. A value that has a counter keeps its counter and its key, and a nil
// counter or key changes nothing, so a caller cannot bind a counted value to a key of its own.
func CountedByKey(c Compared, k Counter, key *StoreKey) Compared {
	if c.counter != nil || k == nil || key == nil {
		return c
	}
	c.counter, c.key = k, key
	return c
}

// ComparedOf gives the Compared of a new text, which the next write stores as its hash; an empty text gives a
// Compared that is not set.
func ComparedOf(text string) Compared {
	if text == "" {
		return Compared{}
	}
	return Compared{text: &text}
}

// ComparedHash gives the Compared of a stored hash, as a read of its column gives it.
func ComparedHash(hash string) Compared {
	if hash == "" {
		return Compared{}
	}
	return Compared{hash: &hash}
}

// IsSet tells whether the value has a new text or a stored hash.
func (c Compared) IsSet() bool { return c.text != nil || c.hash != nil }

// Matches tells whether value is the secret: against the stored hash in constant time, or against a new text that is
// not stored yet, also in constant time. An empty value and a Compared that is not set match nothing.
//
// A stored value of a field with a counter of wrong attempts asks its counter first: when the counter does not allow
// a check, Matches gives false and checks nothing; otherwise the counter records the result (ST-SUP-RP10-LC54).
func (c Compared) Matches(value string) bool {
	if c.counter != nil && c.hash != nil {
		if !c.counter.Charge() {
			// The same work as a check, so the time of the answer does not tell that the limit is reached (security
			// review of PR #451, L1).
			_ = Matches(dummyHash(), value+"\x00")
			return false
		}
		matched := value != "" && Matches(*c.hash, value)
		c.counter.Record(matched)
		return matched
	}
	switch {
	case value == "":
		return false
	case c.hash != nil:
		return Matches(*c.hash, value)
	case c.text != nil:
		return constantEqual(*c.text, value)
	}
	return false
}

// IsNew tells whether c is a new text, which the next write stores as its hash, and not a stored hash.
func (c Compared) IsNew() bool { return c.text != nil }

// Check runs ok on a new text before its hash, such as the generated check of the declared length; a stored hash and
// a value that is not set pass, because their text is not known. ok never gets a stored hash.
func (c Compared) Check(ok func(text string) bool) bool {
	if c.text == nil {
		return true
	}
	return ok(*c.text)
}

// Stored gives the text that the column stores: the hash of a new text, the stored hash, or empty when not set. A
// stored value with a counter of wrong attempts gives ErrCounted: only StoredBy gives its hash.
func (c Compared) Stored() (string, error) { return c.StoredContext(context.Background()) }

// StoredContext is Stored, whose wait for a hash slot ends with ctx.
func (c Compared) StoredContext(ctx context.Context) (string, error) { return c.StoredBy(ctx, nil) }

// StoredBy is StoredContext, which also gives the stored hash of a counted value when key is the store key that
// CountedByKey bound to it.
func (c Compared) StoredBy(ctx context.Context, key *StoreKey) (string, error) {
	if c.counter != nil && c.hash != nil && (key == nil || key != c.key) {
		return "", ErrCounted
	}
	switch {
	case c.text != nil:
		return HashContext(ctx, *c.text)
	case c.hash != nil:
		return *c.hash, nil
	}
	return "", nil
}

// IsCounted tells whether c is a stored value with a counter of wrong attempts, whose hash Stored does not give.
func (c Compared) IsCounted() bool { return c.counter != nil && c.hash != nil }

// CounterBy gives the counter of a stored counted value when key is the store key that CountedByKey bound to it, and
// nil otherwise. The command runtime writes such a value again only into the field and the row of that counter
// (ST-SUP-RP10-LC54; security review of PR #479, L1).
func (c Compared) CounterBy(key *StoreKey) Counter {
	if !c.IsCounted() || key == nil || key != c.key {
		return nil
	}
	return c.counter
}

// String gives [secret].
func (c Compared) String() string { return Redacted }

// GoString gives [secret].
func (c Compared) GoString() string { return Redacted }

// Format writes [secret] for each verb.
func (c Compared) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(Redacted)) }

// LogValue gives [secret] to slog.
func (c Compared) LogValue() slog.Value { return slog.StringValue(Redacted) }

// MarshalJSON encodes the value as null.
func (c Compared) MarshalJSON() ([]byte, error) { return []byte("null"), nil }

// MarshalText encodes the value as [secret].
func (c Compared) MarshalText() ([]byte, error) { return []byte(Redacted), nil }

// UnmarshalJSON refuses each value.
func (c *Compared) UnmarshalJSON([]byte) error { return ErrDecode }
