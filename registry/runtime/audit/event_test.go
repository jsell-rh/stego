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
	"strconv"
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

	t.Run("ST-SUP-RP05-LC03-T08 A JSON value of a jsonb field is redacted at any depth and stored as its JSON value", func(t *testing.T) {
		e := sampleEvent("t1", "update")
		f := Fields{"profile.email": PII, "profile.api_token": Secret}
		before := map[string]any{"profile": json.RawMessage(`{"email":"old@example.com","api_token":"tok-old-1","city":"Oslo","n":1.0}`), "note": json.RawMessage(nil)}
		after := map[string]any{"profile": json.RawMessage(`{"email":"new@example.com","api_token":"tok-new-2","city":"Bergen","n":2}`), "note": json.RawMessage(`[1,2]`)}
		if err := e.SetStates(Settings{Level: LevelChange}, f, before, after); err != nil {
			t.Fatal(err)
		}
		e = linked(t, e)
		if want := `{"note":null,"profile":{"city":"Oslo","email":"` + RedactionMarker + `","n":1}}`; string(e.Before.JSON()) != want {
			t.Errorf("before is %s, want %s", e.Before.JSON(), want)
		}
		if want := `{"note":[1,2],"profile":{"city":"Bergen","email":"` + RedactionMarker + `","n":2}}`; string(e.After.JSON()) != want {
			t.Errorf("after is %s, want %s", e.After.JSON(), want)
		}
		p, err := ParsePatch(e.Patch.JSON())
		if err != nil {
			t.Fatal(err)
		}
		if !slices.ContainsFunc(p, func(o Op) bool { return o.Path == "/profile/email" && o.Value == RedactionMarker }) ||
			!slices.ContainsFunc(p, func(o Op) bool { return o.Path == "/profile/city" }) {
			t.Errorf("the patch %s has no change of /profile/email with the marker and of /profile/city", e.Patch.JSON())
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
		// JSON text that does not decode is refused, and the error names the field and not the value.
		bad := sampleEvent("t1", "update")
		err = bad.SetStates(Settings{Level: LevelChange}, Fields{}, nil, map[string]any{"profile": json.RawMessage(`{"secret-value"`)})
		if err == nil || !strings.Contains(err.Error(), "profile") || strings.Contains(err.Error(), "secret-value") {
			t.Errorf("SetStates with JSON text that does not decode gave %v, want an error that names profile and not the value", err)
		}
	})

	t.Run("ST-SUP-RP05-LC04-T07 A number of a client JSON value costs little, and an overlong one is refused", func(t *testing.T) {
		// PostgreSQL keeps a jsonb number with 16383 fraction digits, so a body of 1 MiB can hold 63 of them.
		build := func(fraction int) json.RawMessage {
			var b strings.Builder
			b.WriteString("[")
			for i := range 63 {
				if i > 0 {
					b.WriteString(",")
				}
				b.WriteString("0." + strings.Repeat("1", fraction-1) + "3")
			}
			b.WriteString("]")
			return json.RawMessage(b.String())
		}
		huge := build(16383)
		start := time.Now()
		e := sampleEvent("t1", "update")
		err := e.SetStates(Settings{Level: LevelChange}, Fields{}, map[string]any{"doc": huge}, map[string]any{"doc": huge})
		if err == nil || !strings.Contains(err.Error(), "doc") || strings.Contains(err.Error(), "1111") {
			t.Errorf("SetStates with numbers of 16383 fraction digits gave %v, want a refusal that names doc and not the value", err)
		}
		// The refusal comes after the JSON text is decoded, which takes about 100 ms under the race detector on CI;
		// the former count of digits took 3.7 s here. The bound is loose so that the race run does not flake.
		if d := time.Since(start); d > time.Second {
			t.Errorf("SetStates with 63 numbers of 16383 fraction digits took %v, want under 1 s", d)
		}
		// The longest accepted number keeps its exact value, and 63 of them in before and after stay cheap.
		long := build(1097)
		start = time.Now()
		e = sampleEvent("t1", "update")
		if err := e.SetStates(Settings{Level: LevelChange}, Fields{}, map[string]any{"doc": long}, map[string]any{"doc": long}); err != nil {
			t.Fatalf("SetStates with numbers of 1099 characters: %v", err)
		}
		// The bound is loose for a run under the race detector; the old count of digits took seconds here.
		if d := time.Since(start); d > time.Second {
			t.Errorf("SetStates with 63 numbers of 1099 characters took %v, want under 1 s", d)
		}
		if want := "0." + strings.Repeat("1", 1096) + "3"; !bytes.Contains(e.After.JSON(), []byte(want)) {
			t.Errorf("the after state does not hold the exact number")
		}
		for text, want := range map[string]string{"1.2500e1": "12.5", "1.25e-3": "0.00125", "-0.10": "-0.1", "5E2": "500", "2.000": "2"} {
			got, err := canonicalNumber("n", text)
			if err != nil || string(got) != want {
				t.Errorf("the canonical form of %s is %q (%v), want %s", text, got, err, want)
			}
		}
	})

	t.Run("ST-SUP-RP05-LC04-T08 A deep JSON value is refused at the depth bound, and a deep update at the bound stays cheap", func(t *testing.T) {
		// nested gives an object of depth levels, each level with one key of 100 characters, and leaf at the bottom.
		nested := func(levels int, leaf string) json.RawMessage {
			key := strings.Repeat("k", 100)
			var b strings.Builder
			for range levels {
				b.WriteString(`{"` + key + `":`)
			}
			b.WriteString(leaf)
			b.WriteString(strings.Repeat("}", levels))
			return json.RawMessage(b.String())
		}
		deep := nested(9000, "1")
		start := time.Now()
		e := sampleEvent("t1", "update")
		err := e.SetStates(Settings{Level: LevelChange}, Fields{}, map[string]any{"doc": deep}, map[string]any{"doc": nested(9000, "2")})
		if err == nil || !strings.Contains(err.Error(), "doc") || !strings.Contains(err.Error(), "64") || strings.Contains(err.Error(), "kkk") {
			t.Errorf("SetStates with a value of depth 9000 gave %v, want a refusal that names doc and the bound and no value", err)
		}
		if d := time.Since(start); d > time.Second {
			t.Errorf("SetStates with a value of depth 9000 took %v, want under 1 s", d)
		}
		e = sampleEvent("t1", "create")
		if err := e.SetStates(Settings{Level: LevelChange}, Fields{}, nil, map[string]any{"doc": nested(65, "1")}); err == nil {
			t.Errorf("SetStates accepts a value of depth 65")
		}
		// At the bound, a large leaf that changes in one member keeps the work linear: the patch compares members,
		// not the encoding of each level.
		leaf := func(last string) string {
			items := make([]string, 0, 30000)
			for i := range 30000 {
				items = append(items, strconv.Itoa(i))
			}
			items[len(items)-1] = last
			return "[" + strings.Join(items, ",") + "]"
		}
		start = time.Now()
		e = sampleEvent("t1", "update")
		if err := e.SetStates(Settings{Level: LevelChange}, Fields{}, map[string]any{"doc": nested(63, leaf("1"))}, map[string]any{"doc": nested(63, leaf("2"))}); err != nil {
			t.Fatalf("SetStates with a value of depth 64: %v", err)
		}
		if d := time.Since(start); d > time.Second {
			t.Errorf("an update of a value of depth 64 took %v, want under 1 s", d)
		}
		p, err := ParsePatch(e.Patch.JSON())
		if err != nil {
			t.Fatal(err)
		}
		if len(p) != 1 || !strings.HasSuffix(p[0].Path, "/29999") || p[0].Op != "replace" {
			t.Errorf("the patch has %d operations, want one replace of the last member: %.200s", len(p), e.Patch.JSON())
		}
		// The patch compares members and not the encoding of each level, so its work is linear also for a value
		// deeper than the bound, which only the internal compare sees.
		tree := func(levels int, leaf any) any {
			v := leaf
			for range levels {
				v = map[string]any{strings.Repeat("k", 100): v}
			}
			return v
		}
		start = time.Now()
		var ops Patch
		diffValue("", tree(5000, json.Number("1")), tree(5000, json.Number("2")), &ops)
		if d := time.Since(start); d > time.Second || len(ops) != 1 || ops[0].Op != "replace" {
			t.Errorf("the compare of two values of depth 5000 took %v and gave %d operations, want one replace in under 1 s", d, len(ops))
		}
	})

	t.Run("ST-SUP-RP05-LC04-T09 A number whose canonical form is long is refused, also when its text is short", func(t *testing.T) {
		if _, err := canonicalNumber("n", "1e-400"); err != nil {
			t.Errorf("the number 1e-400 is refused: %v", err)
		}
		if _, err := canonicalNumber("n", "1"+"."+strings.Repeat("1", 800)+"e-400"); err == nil || !strings.Contains(err.Error(), "n") {
			t.Errorf("a number of 801 mantissa digits and the exponent -400 gave %v, want a refusal that names the field", err)
		}
		// A JSON array of 1 MiB of 1e-400 would give about 60 MB of canonical text: it is refused before any is made.
		items := make([]string, 0, 150000)
		for range 150000 {
			items = append(items, "1e-400")
		}
		raw := json.RawMessage("[" + strings.Join(items, ",") + "]")
		start := time.Now()
		e := sampleEvent("t1", "create")
		err := e.SetStates(Settings{Level: LevelChange}, Fields{}, nil, map[string]any{"doc": raw})
		if err == nil || !strings.Contains(err.Error(), "doc") || strings.Contains(err.Error(), "1e-400") {
			t.Errorf("SetStates with 150000 numbers 1e-400 gave %v, want a refusal that names doc and no value", err)
		}
		if d := time.Since(start); d > time.Second {
			t.Errorf("the refusal took %v, want under 1 s", d)
		}
		// A few such numbers are kept.
		e = sampleEvent("t1", "create")
		if err := e.SetStates(Settings{Level: LevelChange}, Fields{}, nil, map[string]any{"doc": json.RawMessage(`[1e-400, 2.5e3]`)}); err != nil {
			t.Errorf("SetStates with two numbers: %v", err)
		}
	})

	t.Run("ST-SUP-RP05-LC05-T04 The patch of a field stays within a small multiple of the field's size for any shape", func(t *testing.T) {
		long := strings.Repeat("L", 400_000)
		members := func(n int, value string) string {
			items := make([]string, n)
			for i := range items {
				items[i] = `"` + strconv.Itoa(i) + `":` + value
			}
			return strings.Join(items, ",")
		}
		cases := []struct{ name, before, after string }{
			{"1000 adds under a 400 KB key", `{"` + long + `":{}}`, `{"` + long + `":{` + members(1000, "1") + `}}`},
			{"4000 adds under a 400 KB key", `{"` + long + `":{}}`, `{"` + long + `":{` + members(4000, "1") + `}}`},
			{"4000 array adds under a 400 KB key", `{"` + long + `":[]}`, `{"` + long + `":[` + strings.Repeat("1,", 3999) + `1]}`},
			{"4000 replaces under a 400 KB key", `{"` + long + `":[` + strings.Repeat("1,", 3999) + `1]}`, `{"` + long + `":[` + strings.Repeat("2,", 3999) + `2]}`},
			{"1000 changes under 1 KB keys", `{` + strings.Replace(members(1000, "1"), `":`, strings.Repeat("k", 1000)+`":`, -1) + `}`, `{` + strings.Replace(members(1000, "2"), `":`, strings.Repeat("k", 1000)+`":`, -1) + `}`},
		}
		for _, c := range cases {
			start := time.Now()
			e := sampleEvent("t1", "update")
			if err := e.SetStates(Settings{Level: LevelChange}, Fields{}, map[string]any{"doc": json.RawMessage(c.before)}, map[string]any{"doc": json.RawMessage(c.after)}); err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			input := len(c.before) + len(c.after)
			if n := len(e.Patch.JSON()); n > 3*input+4096 {
				t.Errorf("%s: the patch is %d bytes for an input of %d, want at most 3 times the input and 4 KiB", c.name, n, input)
			}
			if d := time.Since(start); d > 2*time.Second {
				t.Errorf("%s took %v, want under 2 s, also under the race detector", c.name, d)
			}
			// The patch applied to before gives after.
			before, _ := decodeJSON("before", e.Before.JSON())
			p, err := ParsePatch(e.Patch.JSON())
			if err != nil {
				t.Fatal(err)
			}
			got, err := Apply(before, p)
			if err != nil {
				t.Fatalf("%s: applying the patch: %v", c.name, err)
			}
			want, _ := decodeJSON("after", e.After.JSON())
			if !bytes.Equal(encodeTree(got), encodeTree(want)) {
				t.Errorf("%s: the patch applied to before does not give after", c.name)
			}
		}
		// A field whose changes are small keeps its member operations.
		e := sampleEvent("t1", "update")
		if err := e.SetStates(Settings{Level: LevelChange}, Fields{}, map[string]any{"doc": json.RawMessage(`{"a":1,"b":2}`)}, map[string]any{"doc": json.RawMessage(`{"a":1,"b":3}`)}); err != nil {
			t.Fatal(err)
		}
		if string(e.Patch.JSON()) != `[{"op":"replace","path":"/doc/b","value":3}]` {
			t.Errorf("the patch of a small change is %s, want one replace of /doc/b", e.Patch.JSON())
		}
	})

	t.Run("ST-SUP-RP05-LC05-T05 Many changed pii values nested in a field cost linear time", func(t *testing.T) {
		build := func(n int, e string, num int) json.RawMessage {
			items := make([]string, n)
			for i := range items {
				items[i] = `{"e":"` + e + `","n":` + strconv.Itoa(num) + `}`
			}
			return json.RawMessage("[" + strings.Join(items, ",") + "]")
		}
		// The time grows with the count as the input does: five times the values take well under 25 times the time,
		// which a cost of the pii values times the operations would take. The ratio holds under the race detector.
		run := func(n int) time.Duration {
			b, a := build(n, "a@example.com", 1), build(n, "b@example.com", 2)
			start := time.Now()
			e := sampleEvent("t1", "update")
			if err := e.SetStates(Settings{Level: LevelChange}, Fields{"m.e": PII}, map[string]any{"m": b}, map[string]any{"m": a}); err != nil {
				t.Fatal(err)
			}
			d := time.Since(start)
			if bytes.Contains(e.Patch.JSON(), []byte("example.com")) {
				t.Error("the patch holds a pii value")
			}
			return d
		}
		run(1000) // warm up
		small, large := run(10_000), run(50_000)
		if large > 10*small+50*time.Millisecond || large > 10*time.Second {
			t.Errorf("50000 changed pii values took %v and 10000 took %v, want linear growth", large, small)
		}
		// Each pii value that changed is marked once, under its own pointer.
		e := sampleEvent("t1", "update")
		if err := e.SetStates(Settings{Level: LevelChange}, Fields{"m.e": PII}, map[string]any{"m": build(3, "a@example.com", 1)}, map[string]any{"m": build(3, "b@example.com", 1)}); err != nil {
			t.Fatal(err)
		}
		p, err := ParsePatch(e.Patch.JSON())
		if err != nil {
			t.Fatal(err)
		}
		if len(p) != 3 || p[0].Path != "/m/0/e" || p[0].Value != RedactionMarker {
			t.Errorf("the patch is %s, want one marker replace for each changed pii value", e.Patch.JSON())
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
