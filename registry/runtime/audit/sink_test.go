package audit

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// fakeSink records what the destination delivers and answers a second Append of an ID with ErrDuplicate.
type fakeSink struct {
	mu      sync.Mutex
	appends [][]Event
	seen    map[UUID]bool
	anchors []Anchor
}

func (s *fakeSink) Append(_ context.Context, events []Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen == nil {
		s.seen = map[UUID]bool{}
	}
	s.appends = append(s.appends, events)
	for _, e := range events {
		if s.seen[e.ID] {
			return fmt.Errorf("event %s: %w", e.ID, ErrDuplicate)
		}
		s.seen[e.ID] = true
	}
	return nil
}

func (s *fakeSink) Anchor(_ context.Context, a Anchor) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.anchors = append(s.anchors, a)
	return nil
}

// of returns the anchors that the sink holds for the tenant, in the order it got them.
func (s *fakeSink) of(tenant string) []Anchor {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Anchor
	for _, a := range s.anchors {
		if a.Tenant == tenant {
			out = append(out, a)
		}
	}
	return out
}

func sinkMessage(t *testing.T) (Event, []byte) {
	t.Helper()
	e := sampleEvent("t1", "update")
	if err := e.SetStates(Settings{Level: LevelChange}, Fields{"email": PII},
		map[string]any{"email": "a@example.com", "n": 1}, map[string]any{"email": "b@example.com", "n": 2}); err != nil {
		t.Fatal(err)
	}
	e = linked(t, e)
	payload, err := eventMessage(e)
	if err != nil {
		t.Fatal(err)
	}
	return e, payload
}

func TestSink(t *testing.T) {
	ctx := context.Background()

	t.Run("ST-SUP-RP05-LC07-T03 The destination delivers the event through the Sink port", func(t *testing.T) {
		e, payload := sinkMessage(t)
		sink := &fakeSink{}
		d, err := NewDestination(sink)
		if err != nil {
			t.Fatal(err)
		}
		if err := d.Deliver(ctx, e.ID.String(), payload); err != nil {
			t.Fatal(err)
		}
		if len(sink.appends) != 1 || len(sink.appends[0]) != 1 {
			t.Fatalf("the sink got %d Append calls, want one with one event", len(sink.appends))
		}
		got, err := Encode(sink.appends[0][0])
		if err != nil {
			t.Fatal(err)
		}
		want, err := Encode(e)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("the sink got\n%s\nwant\n%s", got, want)
		}
	})

	t.Run("ST-SUP-RP05-LC07-T04 A duplicate event ID at the sink is a success", func(t *testing.T) {
		e, payload := sinkMessage(t)
		sink := &fakeSink{}
		d, err := NewDestination(sink)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			if err := d.Deliver(ctx, e.ID.String(), payload); err != nil {
				t.Errorf("delivery %d returns %v, want success", i+1, err)
			}
		}
		if len(sink.appends) != 2 {
			t.Errorf("the sink got %d Append calls, want 2", len(sink.appends))
		}
	})

	t.Run("ST-SUP-RP05-LC08-T01 The Sink port has only Append and Anchor", func(t *testing.T) {
		typ := reflect.TypeOf((*Sink)(nil)).Elem()
		if typ.NumMethod() != 2 {
			t.Fatalf("the Sink interface has %d methods, want 2", typ.NumMethod())
		}
		ctxType := reflect.TypeOf((*context.Context)(nil)).Elem()
		errType := reflect.TypeOf((*error)(nil)).Elem()
		want := map[string][]reflect.Type{
			"Append": {ctxType, reflect.TypeOf([]Event{})},
			"Anchor": {ctxType, reflect.TypeOf(Anchor{})},
		}
		for i := 0; i < typ.NumMethod(); i++ {
			m := typ.Method(i)
			params, ok := want[m.Name]
			if !ok {
				t.Errorf("the Sink interface has the method %s", m.Name)
				continue
			}
			if m.Type.NumIn() != len(params) || m.Type.NumOut() != 1 || m.Type.Out(0) != errType {
				t.Errorf("the method %s has the type %s", m.Name, m.Type)
				continue
			}
			for j, p := range params {
				if m.Type.In(j) != p {
					t.Errorf("the method %s has the parameter %d of type %s, want %s", m.Name, j, m.Type.In(j), p)
				}
			}
		}
		// An anchor holds the tenant, the kind, the head hash and the count, and its message keeps all four.
		var names []string
		at := reflect.TypeOf(Anchor{})
		for i := 0; i < at.NumField(); i++ {
			names = append(names, at.Field(i).Name)
		}
		if strings.Join(names, ",") != "Tenant,Kind,Head,Count" {
			t.Errorf("Anchor has the fields %v, want Tenant, Kind, Head and Count", names)
		}
		sink := &fakeSink{}
		d, _ := NewDestination(sink)
		wantA := Anchor{Tenant: "t1", Kind: AnchorRetention, Head: bytes.Repeat([]byte{7}, 32), Count: 42}
		if err := d.Deliver(ctx, "k", anchorMessage(wantA)); err != nil {
			t.Fatal(err)
		}
		if got := sink.of("t1"); len(got) != 1 || got[0].Kind != wantA.Kind || got[0].Count != 42 || !bytes.Equal(got[0].Head, wantA.Head) {
			t.Errorf("the sink holds %+v, want %+v", got, wantA)
		}
	})

	t.Run("ST-SUP-RP05-LC08-T02 The package exposes no update, delete or read on the sink", func(t *testing.T) {
		// A name that starts with one of these verbs updates, deletes or reads. The methods of Tx are the
		// caller's own database handle, not the sink, and are not part of the scan.
		verbs := regexp.MustCompile(`^(Update|Delete|Remove|Erase|Purge|Truncate|Modify|Edit|Overwrite|Rewrite|Read|Get|Fetch|List|Load|Query|Scan|Find|Lookup|Select|Search|Export|Download|Stream|Tail|Iterate)([A-Z0-9_]|$)`)
		var exported []string
		var sinkParams []string
		for _, file := range packageFiles(t) {
			fset := token.NewFileSet()
			node, err := parser.ParseFile(fset, file, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, decl := range node.Decls {
				switch d := decl.(type) {
				case *ast.FuncDecl:
					if d.Name.IsExported() {
						exported = append(exported, d.Name.Name)
						for _, p := range d.Type.Params.List {
							if id, ok := p.Type.(*ast.Ident); ok && id.Name == "Sink" {
								sinkParams = append(sinkParams, d.Name.Name)
							}
						}
					}
				case *ast.GenDecl:
					for _, spec := range d.Specs {
						ts, ok := spec.(*ast.TypeSpec)
						if !ok || !ts.Name.IsExported() {
							continue
						}
						exported = append(exported, ts.Name.Name)
						if it, ok := ts.Type.(*ast.InterfaceType); ok && ts.Name.Name != "Tx" {
							for _, m := range it.Methods.List {
								for _, n := range m.Names {
									exported = append(exported, ts.Name.Name+"."+n.Name)
								}
							}
						}
					}
				}
			}
		}
		for _, name := range exported {
			short := name[strings.LastIndex(name, ".")+1:]
			if verbs.MatchString(short) {
				t.Errorf("the package exports %s, which updates, deletes or reads", name)
			}
		}
		for _, name := range sinkParams {
			if name != "NewDestination" {
				t.Errorf("the exported function %s takes a Sink: only the outbox destination may reach the sink", name)
			}
		}
	})
}
