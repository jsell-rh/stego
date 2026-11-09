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
			"user_agent", "action", "operation", "on_behalf_of", "resource_type", "resource_id", "outcome", "error_code", "request_id", "trace_id",
			"span_id", "idempotency_key", "before", "after", "patch", "service", "service_version",
			"declaration_version", "hash_version", "prev_hash", "hash"}
		typ := reflect.TypeOf(Event{})
		var got []string
		for i := 0; i < typ.NumField(); i++ {
			name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
			got = append(got, name)
		}
		if len(want) != 28 {
			t.Fatalf("the test lists %d names, want 27", len(want))
		}
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("audit.Event has the JSON names %v, want exactly %v", got, want)
		}
	})

	t.Run("ST-SUP-RP05-LC01-T02 A new audit event ID is a UUID version 7", func(t *testing.T) {
		t0 := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
		a, err := New(sampleEvent("t1", "create"), t0)
		if err != nil {
			t.Fatal(err)
		}
		b, err := New(sampleEvent("t1", "create"), t0.Add(time.Millisecond))
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
		if a.ID.String() >= b.ID.String() {
			t.Errorf("the second ID %s does not sort after the first %s", b.ID, a.ID)
		}
	})

	t.Run("ST-SUP-RP05-LC01-T03 An actor kind outside the four declared kinds is refused", func(t *testing.T) {
		now := time.Now()
		for _, k := range []ActorKind{"user", "service", "system", "impersonated_by"} {
			e := sampleEvent("t1", "create")
			e.ActorKind = k
			if _, err := New(e, now); err != nil {
				t.Errorf("the actor kind %q is refused: %v", k, err)
			}
		}
		for _, k := range []ActorKind{"admin", ""} {
			e := sampleEvent("t1", "create")
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
			e := sampleEvent("t1", "create")
			e.Outcome = o
			if _, err := New(e, now); err != nil {
				t.Errorf("the outcome %q is refused: %v", o, err)
			}
		}
		for _, o := range []Outcome{"success", ""} {
			e := sampleEvent("t1", "create")
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
			for _, p := range untrustedDecisions(t, file, nil) {
				t.Error(p)
			}
		}
		// The scan catches a local copy of an untrusted value, a parameter of the Untrusted type, and a
		// conversion, so it can fail.
		probe := `package audit
func probe(e Event, u Untrusted) bool {
	ip := e.ActorIP
	s := string(ip)
	if s == "10.0.0.1" { return true }
	for u != "" { break }
	return e.UserAgent == "x"
}`
		dir := t.TempDir()
		path := filepath.Join(dir, "probe.go")
		if err := os.WriteFile(path, []byte(probe), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := untrustedDecisions(t, path, []byte(probe)); len(got) != 3 {
			t.Errorf("the scan finds %d decisions in the probe, want 3: %v", len(got), got)
		}
	})

	t.Run("ST-SUP-RP05-LC03-T01 A pii field is redacted in before, after and patch", func(t *testing.T) {
		checkPIIRedacted(t, Settings{Level: LevelChange})
	})

	t.Run("ST-SUP-RP05-LC03-T03 With audit.pii keep a pii field is still redacted in the audit event", func(t *testing.T) {
		checkPIIRedacted(t, Settings{Level: LevelChange, PII: PIIKeep})
		// The setting is read: a value that is neither redact nor keep is refused.
		e := sampleEvent("t1", "update")
		if err := e.SetStates(Settings{Level: LevelChange, PII: "store"}, Fields{"email": PII}, nil, map[string]any{"email": "a@example.com"}); err == nil {
			t.Errorf("SetStates accepts the audit.pii setting store")
		}
	})

	t.Run("ST-SUP-RP05-LC03-T04 A secret field is not in the stored record", func(t *testing.T) {
		e := sampleEvent("t1", "update")
		if err := e.SetStates(Settings{Level: LevelChange}, Fields{"api_key": Secret},
			map[string]any{"name": "a", "api_key": "sk-old-0001"}, map[string]any{"name": "b", "api_key": "sk-new-0002"}); err != nil {
			t.Fatal(err)
		}
		e = linked(t, e)
		for name, s := range map[string]State{"before": e.Before, "after": e.After, "patch": e.Patch} {
			if bytes.Contains(s.JSON(), []byte("api_key")) {
				t.Errorf("%s holds the key api_key: %s", name, s.JSON())
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

	t.Run("ST-SUP-RP05-LC03-T07 A pii or secret field inside a nested value is redacted or dropped", func(t *testing.T) {
		e := sampleEvent("t1", "update")
		f := Fields{"profile.email": PII, "profile.api_token": Secret}
		before := map[string]any{"profile": map[string]any{"email": "old@example.com", "api_token": "tok-old-1", "city": "Oslo"}}
		after := map[string]any{"profile": map[string]any{"email": "new@example.com", "api_token": "tok-new-2", "city": "Bergen"}}
		if err := e.SetStates(Settings{}, f, before, after); err != nil {
			t.Fatal(err)
		}
		e = linked(t, e)
		for name, s := range map[string]State{"before": e.Before, "after": e.After} {
			var m map[string]map[string]any
			if err := json.Unmarshal(s.JSON(), &m); err != nil {
				t.Fatal(err)
			}
			if m["profile"]["email"] != RedactionMarker {
				t.Errorf("%s holds profile.email %v, want the redaction marker", name, m["profile"]["email"])
			}
		}
		p, err := ParsePatch(e.Patch.JSON())
		if err != nil {
			t.Fatal(err)
		}
		if !slices.ContainsFunc(p, func(o Op) bool { return o.Path == "/profile/email" && o.Value == RedactionMarker }) {
			t.Errorf("the patch %s has no replace of /profile/email with the marker", e.Patch.JSON())
		}
		enc, err := Encode(e)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range []string{"api_token", "tok-old-1", "tok-new-2", "old@example.com", "new@example.com"} {
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
		if want := `{"a":{"b":"x y","y":[1,{"c":3,"d":2}]},"m":null,"z":1}`; string(ca) != want || string(cb) != want {
			t.Errorf("the encodings are\n%s\n%s\nwant\n%s", ca, cb, want)
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

	t.Run("ST-SUP-RP05-LC13-T02 No raw state can reach an event", func(t *testing.T) {
		typ := reflect.TypeOf(Event{})
		state := reflect.TypeOf(State{})
		for _, name := range []string{"Before", "After", "Patch"} {
			f, _ := typ.FieldByName(name)
			if f.Type != state {
				t.Errorf("the field %s has the type %s, want State", name, f.Type)
			}
		}
		for i := 0; i < state.NumField(); i++ {
			if state.Field(i).IsExported() {
				t.Errorf("State exports the field %s", state.Field(i).Name)
			}
		}
		if _, ok := reflect.PointerTo(state).MethodByName("UnmarshalJSON"); ok {
			t.Errorf("State has UnmarshalJSON, a way to set raw JSON")
		}
		// In the source, a State that is not empty is made only in SetStates, the redacting path, and in
		// storedState, which reads back a record that the redacting path wrote and keeps its canonical form. No
		// other function writes the bytes of a State, and only the two readers of stored records, scanEvent and
		// eventFrom, call storedState. None of these is exported but SetStates.
		makers := map[string]bool{"SetStates": true, "storedState": true}
		callers := map[string]bool{"scanEvent": true, "eventFrom": true}
		for _, file := range packageFiles(t) {
			fset := token.NewFileSet()
			node, err := parser.ParseFile(fset, file, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, d := range node.Decls {
				fn, ok := d.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				name := fn.Name.Name
				if fn.Recv != nil && len(fn.Recv.List) == 1 {
					if id, ok := fn.Recv.List[0].Type.(*ast.Ident); ok && id.Name == "State" {
						continue // the read methods of State
					}
				}
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					switch x := n.(type) {
					case *ast.CompositeLit:
						if id, ok := x.Type.(*ast.Ident); ok && id.Name == "State" && len(x.Elts) > 0 && !makers[name] {
							t.Errorf("%s: %s makes a State", fset.Position(x.Pos()), name)
						}
					case *ast.AssignStmt:
						for _, l := range x.Lhs {
							if sel, ok := l.(*ast.SelectorExpr); ok && sel.Sel.Name == "raw" {
								t.Errorf("%s: %s writes the bytes of a State", fset.Position(l.Pos()), name)
							}
						}
					case *ast.UnaryExpr:
						if sel, ok := x.X.(*ast.SelectorExpr); ok && x.Op == token.AND && sel.Sel.Name == "raw" {
							t.Errorf("%s: %s takes the address of the bytes of a State", fset.Position(x.Pos()), name)
						}
					case *ast.CallExpr:
						if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "storedState" && !callers[name] {
							t.Errorf("%s: %s calls storedState", fset.Position(x.Pos()), name)
						}
					}
					return true
				})
				if fn.Name.IsExported() && name != "SetStates" && (makers[name] || callers[name]) {
					t.Errorf("%s is exported", name)
				}
			}
		}
	})

	t.Run("ST-SUP-RP05-LC13-T03 An error names no field value", func(t *testing.T) {
		e := sampleEvent("t1", "update")
		err := e.SetStates(Settings{}, Fields{"score": PII}, nil, map[string]any{"score": json.Number("7e999"), "n": 1})
		if err == nil || !strings.Contains(err.Error(), "score") || strings.Contains(err.Error(), "7e999") {
			t.Errorf("the error is %v, want one that names score and not its value", err)
		}
		e = sampleEvent("t1", "update")
		if err := e.SetStates(Settings{}, Fields{"key": Secret}, nil, map[string]any{"key": make(chan int), "n": 1}); err != nil {
			t.Errorf("a secret field of a type with no encoding gives %v, want it dropped with no error", err)
		}
		_, err = Canonical(map[string]any{"plain": json.Number("9e999")})
		if err == nil || strings.Contains(err.Error(), "9e999") {
			t.Errorf("the error is %v, want one that holds no value", err)
		}
	})
}

func checkPIIRedacted(t *testing.T, s Settings) {
	t.Helper()
	e := sampleEvent("t1", "update")
	if err := e.SetStates(s, Fields{"email": PII},
		map[string]any{"name": "Ann", "email": "ann.old@example.com"},
		map[string]any{"name": "Ann", "email": "ann.new@example.com"}); err != nil {
		t.Fatal(err)
	}
	e = linked(t, e)
	for name, st := range map[string]State{"before": e.Before, "after": e.After} {
		var m map[string]any
		if err := json.Unmarshal(st.JSON(), &m); err != nil {
			t.Fatal(err)
		}
		if m["email"] != RedactionMarker {
			t.Errorf("%s holds email %v, want the redaction marker", name, m["email"])
		}
	}
	p, err := ParsePatch(e.Patch.JSON())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(p, func(o Op) bool { return o.Path == "/email" && o.Value == RedactionMarker }) {
		t.Errorf("the patch %s has no operation on email with the redaction marker", e.Patch.JSON())
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

// untrustedDecisions parses one file and returns each condition or comparison that reads an untrusted value: the
// fields ActorIP and UserAgent, a parameter or variable of the type Untrusted, or a local variable that was
// assigned from any of these, also through a conversion. clean, the cleaning of the values, reads them only to
// clean them; it takes a plain string, so its own conditions do not read a tainted name.
func untrustedDecisions(t *testing.T, file string, src []byte) []string {
	t.Helper()
	fset := token.NewFileSet()
	var node *ast.File
	var err error
	if src != nil {
		node, err = parser.ParseFile(fset, file, src, 0)
	} else {
		node, err = parser.ParseFile(fset, file, nil, 0)
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, d := range node.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		tainted := map[string]bool{}
		for _, list := range []*ast.FieldList{fn.Recv, fn.Type.Params} {
			if list == nil {
				continue
			}
			for _, p := range list.List {
				if id, ok := p.Type.(*ast.Ident); ok && id.Name == "Untrusted" {
					for _, n := range p.Names {
						tainted[n.Name] = true
					}
				}
			}
		}
		reads := func(e ast.Expr) bool {
			found := false
			ast.Inspect(e, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.SelectorExpr:
					if x.Sel.Name == "ActorIP" || x.Sel.Name == "UserAgent" {
						found = true
					}
				case *ast.Ident:
					if tainted[x.Name] {
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
		// Taint flows through assignments in source order, twice, so a copy of a copy is caught.
		for pass := 0; pass < 2; pass++ {
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.AssignStmt:
					for i, l := range x.Lhs {
						id, ok := l.(*ast.Ident)
						if !ok {
							continue
						}
						if len(x.Rhs) == len(x.Lhs) && reads(x.Rhs[i]) {
							tainted[id.Name] = true
						}
					}
				case *ast.ValueSpec:
					if id, ok := x.Type.(*ast.Ident); ok && id.Name == "Untrusted" {
						for _, n := range x.Names {
							tainted[n.Name] = true
						}
					}
					for i, n := range x.Names {
						if i < len(x.Values) && reads(x.Values[i]) {
							tainted[n.Name] = true
						}
					}
				}
				return true
			})
		}
		check := func(what string, e ast.Expr) {
			if e != nil && reads(e) {
				out = append(out, fset.Position(e.Pos()).String()+": a "+what+" in "+fn.Name.Name+" reads an untrusted value")
			}
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.IfStmt:
				check("condition of an if statement", x.Cond)
				return true
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
				case token.EQL, token.NEQ, token.LSS, token.LEQ, token.GTR, token.GEQ:
					check("comparison", x)
					return false
				}
			}
			return true
		})
	}
	// An if condition that is itself a comparison is reported twice; keep each position once.
	slices.Sort(out)
	return slices.CompactFunc(out, func(a, b string) bool {
		return strings.SplitN(a, ": ", 2)[0] == strings.SplitN(b, ": ", 2)[0]
	})
}
