// Package spanevent holds the capability to add a probe event to a span of the telemetry runtime (ST-SUP-RP03-LC15,
// ST-SUP-RP12-LC04). otel.Runtime.SpanEvent takes a Key, and only New makes a key that is not zero. Only a package of
// github.com/jsell-rh/stego/registry/runtime can import this package, so code of a service cannot add an event, a name or an attribute to a
// span of the runtime: the probe runtime makes the one key it uses.
package spanevent

// Key is the capability to add a probe event to a span. The zero Key adds nothing.
type Key struct {
	mark *byte
}

// New makes a key, for the probe runtime only.
func New() Key { return Key{mark: new(byte)} }

// Valid tells whether New made the key.
func (k Key) Valid() bool { return k.mark != nil }
