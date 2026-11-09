package audit

import (
	"bytes"
	"strings"
	"testing"
)

func validPointer(p string) bool {
	if p == "" {
		return true
	}
	if p[0] != '/' {
		return false
	}
	for i := 0; i < len(p); i++ {
		if p[i] == '~' && (i+1 >= len(p) || (p[i+1] != '0' && p[i+1] != '1')) {
			return false
		}
	}
	return true
}

func TestPatch(t *testing.T) {
	t.Run("ST-SUP-RP05-LC05-T01 The patch is an RFC 6902 patch from the redacted before to the redacted after", func(t *testing.T) {
		before := map[string]any{"name": "Ann", "old": "x", "email": "ann.old@example.com"}
		after := map[string]any{"name": "Anne", "added": 3, "email": "ann.new@example.com"}
		p, err := Diff(before, after, Fields{"email": PII})
		if err != nil {
			t.Fatal(err)
		}
		marker := false
		for _, o := range p {
			switch o.Op {
			case "add", "remove", "replace", "move", "copy", "test":
			default:
				t.Errorf("the operation %q is not an RFC 6902 operation", o.Op)
			}
			if !validPointer(o.Path) {
				t.Errorf("the path %q is not a valid JSON pointer", o.Path)
			}
			if o.Path == "/email" && o.Value == RedactionMarker {
				marker = true
			}
		}
		if !marker {
			t.Errorf("the patch %v has no redaction marker in place of the pii value", p)
		}
		enc, err := Canonical(p.tree())
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range []string{"ann.old@example.com", "ann.new@example.com"} {
			if bytes.Contains(enc, []byte(v)) {
				t.Errorf("the patch holds the pii value %q: %s", v, enc)
			}
		}
		for _, want := range []string{`"op":"replace","path":"/name"`, `"op":"add","path":"/added"`, `"op":"remove","path":"/old"`} {
			if !containsOp(p, want) {
				t.Errorf("the patch %s has no operation %s", enc, want)
			}
		}
	})

	t.Run("ST-SUP-RP05-LC05-T02 Applying the patch to before gives after exactly", func(t *testing.T) {
		fields := Fields{"email": PII, "token": Secret}
		pairs := []struct {
			name          string
			before, after map[string]any
		}{
			{"nested objects", map[string]any{"a": map[string]any{"b": map[string]any{"c": 1}, "d": "x"}},
				map[string]any{"a": map[string]any{"b": map[string]any{"c": 2, "e": true}, "d": "x"}}},
			{"array grows", map[string]any{"l": []any{1, 2}}, map[string]any{"l": []any{1, 2, 3, map[string]any{"k": "v"}}}},
			{"array shrinks", map[string]any{"l": []any{1, 2, 3, 4}}, map[string]any{"l": []any{9}}},
			{"null values", map[string]any{"n": nil, "m": 1}, map[string]any{"n": 1, "m": nil}},
			{"removed keys", map[string]any{"a": 1, "b": 2, "c": 3}, map[string]any{"b": 2}},
			{"keys with / and ~", map[string]any{"a/b": 1, "c~d": map[string]any{"~1": "x"}},
				map[string]any{"a/b": 2, "c~d": map[string]any{"~1": "y", "/": "z"}}},
			{"pii and secret", map[string]any{"email": "a@example.com", "token": "s1", "x": 1},
				map[string]any{"email": "b@example.com", "token": "s2", "x": 2}},
			{"create", nil, map[string]any{"a": 1}},
			{"delete", map[string]any{"a": 1}, nil},
		}
		for _, c := range pairs {
			p, err := Diff(c.before, c.after, fields)
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			rb, err := Redact(c.before, fields)
			if err != nil {
				t.Fatal(err)
			}
			ra, err := Redact(c.after, fields)
			if err != nil {
				t.Fatal(err)
			}
			var doc, want any
			if rb != nil {
				doc = rb
			}
			if ra != nil {
				want = ra
			}
			got, err := Apply(doc, p)
			if err != nil {
				t.Fatalf("%s: apply: %v", c.name, err)
			}
			cg, _ := Canonical(got)
			cw, _ := Canonical(want)
			if !bytes.Equal(cg, cw) {
				pe, _ := Canonical(p.tree())
				t.Errorf("%s: applying %s gives %s, want %s", c.name, pe, cg, cw)
			}
		}
	})

	t.Run("ST-SUP-RP05-LC05-T03 A metadata-level event carries no before, after or patch", func(t *testing.T) {
		e := sampleEvent("alpha", "update")
		if err := e.SetStates(Settings{Level: LevelMetadata}, nil, map[string]any{"a": 1}, map[string]any{"a": 2}); err != nil {
			t.Fatal(err)
		}
		if e.Before != nil || e.After != nil || e.Patch != nil {
			t.Errorf("a metadata-level event holds before %s, after %s, patch %s", e.Before, e.After, e.Patch)
		}
		enc, err := Encode(linked(t, e))
		if err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{`"before"`, `"after"`, `"patch"`} {
			if bytes.Contains(enc, []byte(k)) {
				t.Errorf("the encoding holds %s: %s", k, enc)
			}
		}
	})
}

func containsOp(p Patch, want string) bool {
	enc, _ := Canonical(p.tree())
	return strings.Contains(string(enc), want)
}
