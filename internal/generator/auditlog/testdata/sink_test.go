package auditlog

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"stego.dev/runtime/audit"
)

// fakeSink is a stand-in for the audit-sink port: it keeps what it is given and reaches no outside system.
type fakeSink struct {
	mu      sync.Mutex
	events  []audit.Event
	anchors []audit.Anchor
}

func (s *fakeSink) Append(_ context.Context, events []audit.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, events...)
	return nil
}

func (s *fakeSink) Anchor(_ context.Context, a audit.Anchor) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.anchors = append(s.anchors, a)
	return nil
}

func TestSink(t *testing.T) {
	t.Run("ST-SUP-RC28-OC08-T02 An anchor run puts the chain head and the row count in the sink", func(t *testing.T) {
		f := newFixture(t)
		w := f.writer()
		var last audit.Event
		for i, plan := range []string{"free", "gold", "free", "gold"} {
			last = f.change(w, "t1", "c1", map[string]any{"plan": "none", "n": i}, map[string]any{"plan": plan, "n": i})
		}
		anchorer, _ := f.jobs("t1")
		if err := anchorer.Tick(f.ctx, time.Now()); err != nil {
			t.Fatal(err)
		}
		// Relay the outbox to the fake sink through the destination of the audit runtime.
		sink := &fakeSink{}
		destination, err := audit.NewDestination(sink)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range f.outbox.all() {
			if m.destination != audit.SinkDestination {
				t.Fatalf("a message for the destination %s, want %s", m.destination, audit.SinkDestination)
			}
			if err := destination.Deliver(f.ctx, m.key, m.payload); err != nil {
				t.Fatal(err)
			}
		}
		if len(sink.anchors) != 1 {
			t.Fatalf("%d anchors in the sink, want one", len(sink.anchors))
		}
		a := sink.anchors[0]
		if a.Tenant != "t1" || a.Count != 4 || !bytes.Equal(a.Head, last.Hash) {
			t.Fatalf("the anchor is tenant %s, count %d, head %x; want t1, 4 and the hash %x of the last row", a.Tenant, a.Count, a.Head, last.Hash)
		}
		if len(sink.events) != 4 {
			t.Fatalf("%d audit rows in the sink, want 4", len(sink.events))
		}
	})
}
