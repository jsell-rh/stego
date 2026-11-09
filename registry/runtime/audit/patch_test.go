package audit

import (
	"bytes"
	"encoding/json"
	"strconv"
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

// testApply is an RFC 6902 applier of the test, apart from the package's Apply, for add, remove and replace on
// documents decoded by encoding/json. It decodes the patch from its JSON text.
func testApply(t *testing.T, doc any, patch []byte) any {
	t.Helper()
	var ops []struct {
		Op    string `json:"op"`
		Path  string `json:"path"`
		Value any    `json:"value"`
	}
	if err := json.Unmarshal(patch, &ops); err != nil {
		t.Fatalf("the patch is not JSON: %v", err)
	}
	for _, o := range ops {
		var parts []string
		if o.Path != "" {
			for _, p := range strings.Split(o.Path[1:], "/") {
				parts = append(parts, strings.ReplaceAll(strings.ReplaceAll(p, "~1", "/"), "~0", "~"))
			}
		}
		doc = testSet(t, doc, parts, o.Op, o.Value)
	}
	return doc
}

func testSet(t *testing.T, doc any, parts []string, op string, value any) any {
	t.Helper()
	if len(parts) == 0 {
		if op == "remove" {
			return nil
		}
		return value
	}
	k := parts[0]
	switch c := doc.(type) {
	case map[string]any:
		if len(parts) == 1 {
			if op == "remove" {
				delete(c, k)
			} else {
				c[k] = value
			}
			return c
		}
		c[k] = testSet(t, c[k], parts[1:], op, value)
		return c
	case []any:
		i, err := strconv.Atoi(k)
		if err != nil {
			t.Fatalf("the index %q is not a number", k)
		}
		if len(parts) == 1 {
			switch op {
			case "add":
				return append(c[:i], append([]any{value}, c[i:]...)...)
			case "remove":
				return append(c[:i], c[i+1:]...)
			default:
				c[i] = value
				return c
			}
		}
		c[i] = testSet(t, c[i], parts[1:], op, value)
		return c
	default:
		t.Fatalf("the path goes through a scalar")
		return nil
	}
}

func jsonValue(t *testing.T, s string) any {
	t.Helper()
	if s == "" {
		return nil
	}
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func stateOf(t *testing.T, s string) map[string]any {
	t.Helper()
	if s == "" {
		return nil
	}
	d := json.NewDecoder(strings.NewReader(s))
	d.UseNumber()
	var m map[string]any
	if err := d.Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestPatch(t *testing.T) {
	t.Run("ST-SUP-RP05-LC05-T01 The patch is an RFC 6902 patch from the redacted before to the redacted after", func(t *testing.T) {
		before := map[string]any{"name": "Ann", "old": "x", "email": "ann.old@example.com"}
		after := map[string]any{"name": "Anne", "added": 3, "email": "ann.new@example.com"}
		p, err := Diff(before, after, Fields{"email": PII})
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range p {
			switch o.Op {
			case "add", "remove", "replace", "move", "copy", "test":
			default:
				t.Errorf("the operation %q is not an RFC 6902 operation", o.Op)
			}
			if !validPointer(o.Path) {
				t.Errorf("the path %q is not a valid JSON pointer", o.Path)
			}
		}
		got := string(encodeTree(p.tree()))
		want := `[{"op":"add","path":"/added","value":3},{"op":"replace","path":"/name","value":"Anne"},{"op":"remove","path":"/old"},{"op":"replace","path":"/email","value":"[redacted]"}]`
		if got != want {
			t.Errorf("the patch is\n%s\nwant\n%s", got, want)
		}
	})

	t.Run("ST-SUP-RP05-LC05-T02 Applying the patch to before gives after exactly", func(t *testing.T) {
		fields := Fields{"email": PII, "token": Secret}
		cases := []struct {
			name, before, after, golden string
		}{
			{"nested objects", `{"a":{"b":{"c":1},"d":"x"}}`, `{"a":{"b":{"c":2,"e":true},"d":"x"}}`,
				`[{"op":"replace","path":"/a/b/c","value":2},{"op":"add","path":"/a/b/e","value":true}]`},
			{"array grows", `{"l":[1,2]}`, `{"l":[1,2,3,{"k":"v"}]}`,
				`[{"op":"add","path":"/l/2","value":3},{"op":"add","path":"/l/3","value":{"k":"v"}}]`},
			{"array shrinks", `{"l":[1,2,3,4]}`, `{"l":[9]}`,
				`[{"op":"replace","path":"/l/0","value":9},{"op":"remove","path":"/l/3"},{"op":"remove","path":"/l/2"},{"op":"remove","path":"/l/1"}]`},
			{"null values", `{"m":1,"n":null}`, `{"m":null,"n":1}`,
				`[{"op":"replace","path":"/m","value":null},{"op":"replace","path":"/n","value":1}]`},
			{"removed keys", `{"a":1,"b":2,"c":3}`, `{"b":2}`,
				`[{"op":"remove","path":"/a"},{"op":"remove","path":"/c"}]`},
			{"keys with / and ~", `{"a/b":1,"c~d":{"~1":"x"}}`, `{"a/b":2,"c~d":{"/":"z","~1":"y"}}`,
				`[{"op":"replace","path":"/a~1b","value":2},{"op":"add","path":"/c~0d/~1","value":"z"},{"op":"replace","path":"/c~0d/~01","value":"y"}]`},
			{"pii and secret", `{"email":"a@example.com","token":"s1","x":1}`, `{"email":"b@example.com","token":"s2","x":2}`,
				`[{"op":"replace","path":"/x","value":2},{"op":"replace","path":"/email","value":"[redacted]"}]`},
			{"create", ``, `{"a":1}`, `[{"op":"replace","path":"","value":{"a":1}}]`},
			{"delete", `{"a":1}`, ``, `[{"op":"replace","path":"","value":null}]`},
		}
		for _, c := range cases {
			p, err := Diff(stateOf(t, c.before), stateOf(t, c.after), fields)
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			got := encodeTree(p.tree())
			if string(got) != c.golden {
				t.Errorf("%s: the patch is\n%s\nwant the golden patch\n%s", c.name, got, c.golden)
				continue
			}
			rb, _ := Redact(stateOf(t, c.before), fields)
			ra, _ := Redact(stateOf(t, c.after), fields)
			var doc any
			if rb != nil {
				doc = jsonValue(t, string(encodeTree(rb)))
			}
			result := testApply(t, doc, got)
			wantJSON := "null"
			if ra != nil {
				wantJSON = string(encodeTree(ra))
			}
			gotJSON, _ := json.Marshal(result)
			if !jsonEqual(t, gotJSON, []byte(wantJSON)) {
				t.Errorf("%s: applying the patch gives %s, want %s", c.name, gotJSON, wantJSON)
			}
		}
	})

	t.Run("ST-SUP-RP05-LC05-T03 A metadata-level event carries no before, after or patch", func(t *testing.T) {
		e := sampleEvent("t1", "update")
		if err := e.SetStates(Settings{Level: LevelMetadata}, nil, map[string]any{"a": 1}, map[string]any{"a": 2}); err != nil {
			t.Fatal(err)
		}
		if !e.Before.IsZero() || !e.After.IsZero() || !e.Patch.IsZero() {
			t.Errorf("a metadata-level event holds before %s, after %s, patch %s", e.Before.JSON(), e.After.JSON(), e.Patch.JSON())
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

func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	ca, err := Canonical(jsonValue(t, string(a)))
	if err != nil {
		t.Fatal(err)
	}
	cb, err := Canonical(jsonValue(t, string(b)))
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Equal(ca, cb)
}
