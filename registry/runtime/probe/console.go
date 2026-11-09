package probe

import (
	"context"
	"io"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"
)

// format is the console form of declared observations.
type format uint8

const (
	formatOff format = iota
	formatJSON
	formatLogfmt
)

// consoleSink writes one line for each record to standard output through its bounded queue. It starts no
// OpenTelemetry provider and opens no connection (ST-SUP-RP12-LC18). The built-in service and worker events are
// on it in every setting, as one JSON line (ST-SUP-RP12-LC22); declared observations and the schema backfill
// observations only when STEGO_PROBE_CONSOLE turns it on (ST-SUP-RP12-LC16).
type consoleSink struct {
	queue    *queue
	builtins *queue
	mu       sync.Mutex
	out      io.Writer
	format   format
	service  string
	instance string
	line     []byte
}

func newConsoleSink(out io.Writer, f format, service, instance string) *consoleSink {
	s := &consoleSink{out: out, format: f, service: service, instance: instance}
	s.queue = newQueue(s.write)
	s.builtins = newBoundedQueue(BuiltinQueueLimit, s.write)
	return s
}

// BuiltinQueueLimit is the most built-in service and worker events that the console holds in their own queue, so
// a full queue of declared observations never drops one (ST-SUP-RP12-LC22).
const BuiltinQueueLimit = 64

// send queues the record when the console takes it: a built-in service or worker event in its own queue.
func (s *consoleSink) send(r *record) {
	switch {
	case r.obs.class == serviceClass || r.obs.class == workerClass:
		s.builtins.put(r)
	case s.formatOf(r) != formatOff:
		s.queue.put(r)
	}
}

func (s *consoleSink) dropped() uint64 { return s.queue.dropped.Load() + s.builtins.dropped.Load() }
func (s *consoleSink) failed() uint64  { return s.queue.failed.Load() + s.builtins.failed.Load() }

// drain writes what both queues hold, then the line of telemetry.runtime.stopped, all until ctx ends. It tells
// whether it wrote everything.
func (s *consoleSink) drain(ctx context.Context, stopped *record) bool {
	ok := s.queue.drain(ctx)
	if !s.builtins.drain(ctx) {
		ok = false
	}
	if !ok {
		return false
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if !safeWrite(s.write, stopped) {
			s.builtins.failed.Add(1)
		}
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

func (s *consoleSink) formatOf(r *record) format {
	switch r.obs.class {
	case serviceClass, workerClass:
		return formatJSON
	}
	return s.format
}

// write writes the line of one record in one write call. The two queues share the buffer and the writer.
func (s *consoleSink) write(r *record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.formatOf(r) == formatLogfmt {
		s.line = s.logfmt(s.line[:0], r)
	} else {
		s.line = s.json(s.line[:0], r)
	}
	_, err := s.out.Write(s.line)
	return err
}

// json is the JSON line of a record: the timestamp, the severity, the service name, the instance identity, the
// name, the stego.outcome, the histogram or count value, the correlation and the declared fields, in that order
// (ST-SUP-RP12-LC07). A built-in line carries no tenant, principal or operation (ST-SUP-RP12-LC22).
func (s *consoleSink) json(b []byte, r *record) []byte {
	b = append(b, `{"timestamp":`...)
	b = appendJSONString(b, r.time.UTC().Format(time.RFC3339Nano))
	b = append(b, `,"severity":`...)
	b = appendJSONString(b, r.obs.severity.String())
	b = append(b, `,"service.name":`...)
	b = appendJSONString(b, s.service)
	b = append(b, `,"service.instance.id":`...)
	b = appendJSONString(b, s.instance)
	b = append(b, `,"event.name":`...)
	b = appendJSONString(b, r.obs.signal)
	if r.outcome != "" {
		b = append(b, `,"stego.outcome":`...)
		b = appendJSONString(b, r.outcome)
	}
	if text, ok := consoleValue(r); ok {
		b = append(b, `,"value":`...)
		b = append(b, text...)
	}
	if r.spanCtx.IsValid() {
		b = append(b, `,"trace_id":`...)
		b = appendJSONString(b, r.spanCtx.TraceID().String())
		b = append(b, `,"span_id":`...)
		b = appendJSONString(b, r.spanCtx.SpanID().String())
	}
	if r.obs.class == declared {
		b = append(b, `,"tenant":`...)
		b = appendJSONString(b, r.tenant)
		b = append(b, `,"principal":`...)
		b = appendJSONString(b, r.principal)
		b = append(b, `,"operation":`...)
		b = appendJSONString(b, r.operation)
	}
	if len(r.obs.fields) > 0 {
		b = append(b, `,"fields":{`...)
		for i, f := range r.obs.fields {
			if i > 0 {
				b = append(b, ',')
			}
			b = appendJSONString(b, f.name)
			b = append(b, ':')
			b = appendJSONValue(b, r.values[i])
		}
		b = append(b, '}')
	}
	return append(b, '}', '\n')
}

// logfmt is the logfmt line of a record, with the keys of the JSON line and each declared field as fields.<name>.
func (s *consoleSink) logfmt(b []byte, r *record) []byte {
	pair := func(b []byte, key, value string) []byte {
		if len(b) > 0 {
			b = append(b, ' ')
		}
		b = append(b, key...)
		b = append(b, '=')
		return appendLogfmtValue(b, value)
	}
	b = pair(b, "timestamp", r.time.UTC().Format(time.RFC3339Nano))
	b = pair(b, "severity", r.obs.severity.String())
	b = pair(b, "service.name", s.service)
	b = pair(b, "service.instance.id", s.instance)
	b = pair(b, "event.name", r.obs.signal)
	if r.outcome != "" {
		b = pair(b, "stego.outcome", r.outcome)
	}
	if text, ok := consoleValue(r); ok {
		b = pair(b, "value", string(text))
	}
	if r.spanCtx.IsValid() {
		b = pair(b, "trace_id", r.spanCtx.TraceID().String())
		b = pair(b, "span_id", r.spanCtx.SpanID().String())
	}
	if r.obs.class == declared {
		b = pair(b, "tenant", r.tenant)
		b = pair(b, "principal", r.principal)
		b = pair(b, "operation", r.operation)
	}
	for i, f := range r.obs.fields {
		b = pair(b, "fields."+f.name, r.values[i].String())
	}
	return append(b, '\n')
}

// consoleValue is the histogram value of a record, or the count that a built-in backfill counter adds.
func consoleValue(r *record) ([]byte, bool) {
	switch {
	case r.hasNumber:
		return strconv.AppendFloat(nil, r.number, 'g', -1, 64), true
	case r.obs.class == backfillClass && r.obs.kind == Counter:
		return strconv.AppendInt(nil, r.count, 10), true
	}
	return nil, false
}

func appendJSONValue(b []byte, v Value) []byte {
	switch v.kind {
	case integerValue, measureValue:
		return strconv.AppendInt(b, v.number, 10)
	case boolValue:
		return strconv.AppendBool(b, v.flag)
	}
	return appendJSONString(b, v.text)
}

// appendJSONString appends text as a JSON string. Text is valid UTF-8 here; an invalid byte becomes U+FFFD.
func appendJSONString(b []byte, text string) []byte {
	const hex = "0123456789abcdef"
	b = append(b, '"')
	for i := 0; i < len(text); {
		c := text[i]
		if c < utf8.RuneSelf {
			switch {
			case c == '"' || c == '\\':
				b = append(b, '\\', c)
			case c == '\n':
				b = append(b, '\\', 'n')
			case c == '\r':
				b = append(b, '\\', 'r')
			case c == '\t':
				b = append(b, '\\', 't')
			case c < 0x20 || c == 0x7f:
				b = append(b, '\\', 'u', '0', '0', hex[c>>4], hex[c&0xf])
			default:
				b = append(b, c)
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(text[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			b = append(b, `�`...)
		case r == ' ' || r == ' ':
			b = append(b, '\\', 'u', '2', '0', '2', hex[r&0xf])
		default:
			b = append(b, text[i:i+size]...)
		}
		i += size
	}
	return append(b, '"')
}

// appendLogfmtValue appends a logfmt value, quoted when it is empty or holds a space, a quote, an equals sign or a
// control character.
func appendLogfmtValue(b []byte, value string) []byte {
	plain := value != ""
	for i := 0; i < len(value) && plain; i++ {
		c := value[i]
		if c <= ' ' || c == '"' || c == '=' || c == '\\' || c == 0x7f || c >= utf8.RuneSelf {
			plain = false
		}
	}
	if plain {
		return append(b, value...)
	}
	return appendJSONString(b, value)
}
