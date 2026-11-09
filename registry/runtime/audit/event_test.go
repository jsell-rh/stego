package audit

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestEvent(t *testing.T) {
	t.Run("ST-SUP-RP05-LC01-T01 An audit event has exactly the declared fields", func(t *testing.T) {
		want := []string{"id", "occurred_at", "recorded_at", "tenant_id", "actor_id", "actor_kind", "actor_ip",
			"user_agent", "action", "resource_type", "resource_id", "outcome", "error_code", "request_id", "trace_id",
			"span_id", "idempotency_key", "before", "after", "patch", "service", "service_version",
			"declaration_version", "prev_hash", "hash"}
		typ := reflect.TypeOf(Event{})
		var got []string
		for i := 0; i < typ.NumField(); i++ {
			name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
			got = append(got, name)
		}
		if len(want) != 25 {
			t.Fatalf("the test lists %d names, want 25", len(want))
		}
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("audit.Event has the JSON names %v, want exactly %v", got, want)
		}
	})

	t.Run("ST-SUP-RP05-LC01-T02 A new audit event ID is a UUID version 7", func(t *testing.T) {
		t0 := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
		a, err := New(sampleEvent("alpha", "create"), t0)
		if err != nil {
			t.Fatal(err)
		}
		b, err := New(sampleEvent("alpha", "create"), t0.Add(time.Millisecond))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range []Event{a, b} {
			s := e.ID.String()
			raw, err := parseUUIDText(s)
			if err != nil {
				t.Fatalf("the ID %s does not parse: %v", s, err)
			}
			if raw[6]>>4 != 7 {
				t.Errorf("the ID %s has version %d, want 7", s, raw[6]>>4)
			}
			if raw[8]>>6 != 0b10 {
				t.Errorf("the ID %s does not have the RFC 9562 variant", s)
			}
		}
		if a.ID.String() >= b.ID.String() || bytes.Compare(a.ID[:], b.ID[:]) >= 0 {
			t.Errorf("the second ID %s does not sort after the first %s", b.ID, a.ID)
		}
	})

	t.Run("ST-SUP-RP05-LC01-T03 An actor kind outside the four declared kinds is refused", func(t *testing.T) {
		now := time.Now()
		for _, k := range []ActorKind{"user", "service", "system", "impersonated_by"} {
			e := sampleEvent("alpha", "create")
			e.ActorKind = k
			if _, err := New(e, now); err != nil {
				t.Errorf("the actor kind %q is refused: %v", k, err)
			}
		}
		for _, k := range []ActorKind{"admin", ""} {
			e := sampleEvent("alpha", "create")
			e.ActorKind = k
			_, err := New(e, now)
			if err == nil || !strings.Contains(err.Error(), "actor_kind") {
				t.Errorf("the actor kind %q gives %v, want an error that names actor_kind", k, err)
			}
		}
	})

	t.Run("ST-SUP-RP05-LC01-T04 An outcome outside ok, denied and failed is refused", func(t *testing.T) {
		now := time.Now()
		for _, o := range []Outcome{"ok", "denied", "failed"} {
			e := sampleEvent("alpha", "create")
			e.Outcome = o
			if _, err := New(e, now); err != nil {
				t.Errorf("the outcome %q is refused: %v", o, err)
			}
		}
		for _, o := range []Outcome{"success", ""} {
			e := sampleEvent("alpha", "create")
			e.Outcome = o
			_, err := New(e, now)
			if err == nil || !strings.Contains(err.Error(), "outcome") {
				t.Errorf("the outcome %q gives %v, want an error that names outcome", o, err)
			}
		}
	})

	t.Run("ST-SUP-RP05-LC01-T05 The actor address and the user agent are marked untrusted", func(t *testing.T) {
		typ := reflect.TypeOf(Event{})
		untrusted := reflect.TypeOf(Untrusted(""))
		for _, name := range []string{"ActorIP", "UserAgent"} {
			f, ok := typ.FieldByName(name)
			if !ok {
				t.Fatalf("audit.Event has no field %s", name)
			}
			if f.Type != untrusted {
				t.Errorf("the field %s has the type %s, want %s", name, f.Type, untrusted)
			}
		}
	})

	t.Run("ST-SUP-RP05-LC01-T06 No code of the package decides on the actor address or the user agent", func(t *testing.T) {
		for _, file := range packageFiles(t) {
			fset := token.NewFileSet()
			node, err := parser.ParseFile(fset, file, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			check := func(what string, e ast.Expr) {
				if e != nil && readsUntrusted(e) {
					t.Errorf("%s: a %s reads actor_ip or user_agent", fset.Position(e.Pos()), what)
				}
			}
			ast.Inspect(node, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.IfStmt:
					check("condition of an if statement", x.Cond)
				case *ast.ForStmt:
					check("condition of a for statement", x.Cond)
				case *ast.SwitchStmt:
					check("switch tag", x.Tag)
					for _, c := range x.Body.List {
						for _, e := range c.(*ast.CaseClause).List {
							check("case of a switch statement", e)
						}
					}
				case *ast.BinaryExpr:
					switch x.Op {
					case token.EQL, token.NEQ, token.LSS, token.LEQ, token.GTR, token.GEQ, token.LAND, token.LOR:
						check("comparison", x)
					}
				}
				return true
			})
		}
	})

	t.Run("ST-SUP-RP05-LC03-T01 A pii field is redacted in before, after and patch", func(t *testing.T) {
		checkPIIRedacted(t, Settings{Level: LevelChange})
	})

	t.Run("ST-SUP-RP05-LC03-T03 With audit.pii keep a pii field is still redacted in the audit event", func(t *testing.T) {
		checkPIIRedacted(t, Settings{Level: LevelChange, PII: PIIKeep})
	})

	t.Run("ST-SUP-RP05-LC03-T04 A secret field is not in the stored record", func(t *testing.T) {
		e := sampleEvent("alpha", "update")
		if err := e.SetStates(Settings{Level: LevelChange}, Fields{"api_key": Secret},
			map[string]any{"name": "a", "api_key": "sk-old-0001"}, map[string]any{"name": "b", "api_key": "sk-new-0002"}); err != nil {
			t.Fatal(err)
		}
		e = linked(t, e)
		for name, raw := range map[string][]byte{"before": e.Before, "after": e.After, "patch": e.Patch} {
			if bytes.Contains(raw, []byte("api_key")) {
				t.Errorf("%s holds the key api_key: %s", name, raw)
			}
		}
		enc, err := Encode(e)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range []string{"sk-old-0001", "sk-new-0002", "api_key"} {
			if bytes.Contains(enc, []byte(v)) {
				t.Errorf("the encoded record holds %q", v)
			}
		}
	})

	t.Run("ST-SUP-RP05-LC04-T01 The canonical encoding sorts keys and has no insignificant whitespace", func(t *testing.T) {
		a := decodeText(t, `{"z": 1, "a": {"y": [1, {"d": 2, "c": 3}], "b": "x y"}, "m": null}`)
		b := decodeText(t, "{\n  \"m\": null,\n  \"a\": {\"b\": \"x y\", \"y\": [1, {\"c\": 3, \"d\": 2}]},\n  \"z\": 1\n}")
		ca, err := Canonical(a)
		if err != nil {
			t.Fatal(err)
		}
		cb, err := Canonical(b)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(ca, cb) {
			t.Fatalf("the encodings differ:\n%s\n%s", ca, cb)
		}
		checkSortedKeys(t, ca)
		inString, escaped := false, false
		for i, c := range ca {
			switch {
			case escaped:
				escaped = false
			case c == '\\' && inString:
				escaped = true
			case c == '"':
				inString = !inString
			case !inString && (c == ' ' || c == '\n' || c == '\t' || c == '\r'):
				t.Errorf("whitespace outside a string at byte %d: %s", i, ca)
			}
		}
	})

	t.Run("ST-SUP-RP05-LC04-T02 Numbers and times have one canonical form", func(t *testing.T) {
		utc := time.Date(2026, 3, 20, 10, 30, 0, 123456000, time.UTC)
		plus2 := utc.In(time.FixedZone("UTC+02:00", 2*3600))
		pairs := []struct {
			a, b   any
			golden string
		}{
			{map[string]any{"n": json.Number("1")}, map[string]any{"n": json.Number("1.0")}, `{"n":1}`},
			{map[string]any{"n": 1}, map[string]any{"n": 1.0}, `{"n":1}`},
			{map[string]any{"n": json.Number("2.50")}, map[string]any{"n": json.Number("25e-1")}, `{"n":2.5}`},
			{map[string]any{"t": utc}, map[string]any{"t": plus2}, `{"t":"2026-03-20T10:30:00.123456Z"}`},
		}
		for _, p := range pairs {
			ca, err := Canonical(p.a)
			if err != nil {
				t.Fatal(err)
			}
			cb, err := Canonical(p.b)
			if err != nil {
				t.Fatal(err)
			}
			if string(ca) != p.golden || string(cb) != p.golden {
				t.Errorf("the encodings are %s and %s, want both %s", ca, cb, p.golden)
			}
		}
	})
}

func checkPIIRedacted(t *testing.T, s Settings) {
	t.Helper()
	e := sampleEvent("alpha", "update")
	if err := e.SetStates(s, Fields{"email": PII},
		map[string]any{"name": "Ann", "email": "ann.old@example.com"},
		map[string]any{"name": "Ann", "email": "ann.new@example.com"}); err != nil {
		t.Fatal(err)
	}
	e = linked(t, e)
	for name, raw := range map[string][]byte{"before": e.Before, "after": e.After} {
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		if m["email"] != RedactionMarker {
			t.Errorf("%s holds email %v, want the redaction marker", name, m["email"])
		}
	}
	p, err := ParsePatch(e.Patch)
	if err != nil {
		t.Fatal(err)
	}
	marked := false
	for _, o := range p {
		if o.Path == "/email" && o.Value == RedactionMarker {
			marked = true
		}
	}
	if !marked {
		t.Errorf("the patch %s has no operation on email with the redaction marker", e.Patch)
	}
	enc, err := Encode(e)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"ann.old@example.com", "ann.new@example.com"} {
		if bytes.Contains(enc, []byte(v)) {
			t.Errorf("the encoded record holds %q", v)
		}
	}
}

func linked(t *testing.T, e Event) Event {
	t.Helper()
	e, err := New(e, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	e.RecordedAt = e.OccurredAt
	e, err = Link(e, ZeroHash())
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func decodeText(t *testing.T, s string) any {
	t.Helper()
	d := json.NewDecoder(strings.NewReader(s))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// checkSortedKeys walks the tokens of an encoding and fails when the keys of an object are not in sorted order.
func checkSortedKeys(t *testing.T, enc []byte) {
	t.Helper()
	type frame struct {
		object, wantKey, seen bool
		last                  string
	}
	var stack []*frame
	valueDone := func() {
		if len(stack) > 0 && stack[len(stack)-1].object {
			stack[len(stack)-1].wantKey = true
		}
	}
	d := json.NewDecoder(bytes.NewReader(enc))
	for {
		tok, err := d.Token()
		if err != nil {
			return
		}
		if len(stack) > 0 && stack[len(stack)-1].object && stack[len(stack)-1].wantKey {
			top := stack[len(stack)-1]
			if tok == json.Delim('}') {
				stack = stack[:len(stack)-1]
				valueDone()
				continue
			}
			k := tok.(string)
			if top.seen && k <= top.last {
				t.Errorf("the key %q follows %q", k, top.last)
			}
			top.last, top.seen, top.wantKey = k, true, false
			continue
		}
		switch tok {
		case json.Delim('{'):
			stack = append(stack, &frame{object: true, wantKey: true})
		case json.Delim('['):
			stack = append(stack, &frame{})
		case json.Delim(']'):
			stack = stack[:len(stack)-1]
			valueDone()
		default:
			valueDone()
		}
	}
}

// parseUUIDText parses a UUID text by itself, apart from the package's own parser.
func parseUUIDText(s string) ([16]byte, error) {
	var out [16]byte
	h := strings.ReplaceAll(s, "-", "")
	if len(s) != 36 || len(h) != 32 {
		return out, os.ErrInvalid
	}
	for i := 0; i < 16; i++ {
		var v byte
		for _, c := range h[2*i : 2*i+2] {
			v <<= 4
			switch {
			case c >= '0' && c <= '9':
				v |= byte(c - '0')
			case c >= 'a' && c <= 'f':
				v |= byte(c-'a') + 10
			default:
				return out, os.ErrInvalid
			}
		}
		out[i] = v
	}
	return out, nil
}

// packageFiles lists the non-test Go files of the package.
func packageFiles(t *testing.T) []string {
	t.Helper()
	all, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range all {
		if !strings.HasSuffix(f, "_test.go") {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		t.Fatal("no source files found")
	}
	return out
}

func readsUntrusted(e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			if x.Sel.Name == "ActorIP" || x.Sel.Name == "UserAgent" {
				found = true
			}
		case *ast.Ident:
			if x.Name == "ActorIP" || x.Name == "UserAgent" {
				found = true
			}
		case *ast.BasicLit:
			if x.Kind == token.STRING && (strings.Contains(x.Value, "actor_ip") || strings.Contains(x.Value, "user_agent")) {
				found = true
			}
		}
		return !found
	})
	return found
}
