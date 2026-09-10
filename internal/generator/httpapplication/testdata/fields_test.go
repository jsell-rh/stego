package sample

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"example.com/http-test/out/application/transport"
)

func fieldProjector(t testing.TB) *transport.FieldProjector {
	t.Helper()
	p, err := transport.NewFieldProjector(transport.FieldSchema{"id": nil, "name": nil, "amount": nil, "tags": nil, "profile": {"name": nil, "active": nil}, "members": {"name": nil, "active": nil}})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestProjectionPreservesJSONAndEnvelope(t *testing.T) {
	p := fieldProjector(t)
	data := json.RawMessage(`{"kind":"RecordList","total":9007199254740993,"items":[{"id":"one","amount":9007199254740993,"profile":{"name":"A","active":false,"password":"hidden"},"members":[{"name":"B","active":true,"password":"hidden"},null],"tags":["a","b"],"secret":"hidden"}]}`)
	for _, query := range []string{"", "*", "id,amount,profile.*,members.*,tags"} {
		selection, err := p.Parse(query)
		if err != nil {
			t.Fatal(err)
		}
		result, err := selection.ProjectList(data, "items")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(result), "hidden") || strings.Count(string(result), "9007199254740993") != 2 || !strings.Contains(string(result), `"active":false`) || !strings.Contains(string(result), `"tags":["a","b"]`) {
			t.Fatal("projection changed values or exposed undeclared data", string(result))
		}
	}
	selection, _ := p.Parse("profile.name,members.name")
	result, err := selection.ProjectList(data, "items")
	if err != nil {
		t.Fatal(err)
	}
	want := `{"items":[{"members":[{"name":"B"},null],"profile":{"name":"A"}}],"kind":"RecordList","total":9007199254740993}`
	if string(result) != want {
		t.Fatalf("nested projection: %s", result)
	}
	result, err = selection.ProjectList(json.RawMessage(`{"items":[],"total":0}`), "items")
	if err != nil || string(result) != `{"items":[],"total":0}` {
		t.Fatal("empty array changed", string(result), err)
	}
	result, err = selection.ProjectList(json.RawMessage(`{"items":[{"profile":null},{"id":"absent"}],"total":2}`), "items")
	if err != nil || string(result) != `{"items":[{"profile":null},{}],"total":2}` {
		t.Fatal("null or absence changed", string(result), err)
	}
}

func TestProjectionRejectsSelectorsWithoutData(t *testing.T) {
	p := fieldProjector(t)
	for _, query := range []string{"unknown", "profile.unknown", "profile.name.more", "name.*", "*,unknown", "name,,id", "name,name", "name, name", ".name", "profile.", "profile.*.name", "id;DROP", "Name", strings.Repeat("a", 4097), strings.Repeat("name,", 64) + "id", "\x00", "\xff"} {
		if _, err := p.Parse(query); !errors.Is(err, transport.ErrRequest) {
			t.Fatalf("invalid selector accepted: %q: %v", query, err)
		}
	}
	for _, query := range []string{"profile,profile.name", "profile.name,profile", "*,profile.name"} {
		selection, err := p.Parse(query)
		if err != nil {
			t.Fatal(err)
		}
		data, err := selection.ProjectList(json.RawMessage(`{"items":[{"profile":{"name":"A","active":false,"hidden":"secret"}}]}`), "items")
		if err != nil || strings.Contains(string(data), "secret") || !strings.Contains(string(data), `"active":false`) {
			t.Fatal("overlap changed selection", string(data), err)
		}
	}
}

func TestProjectionRejectsMalformedResponses(t *testing.T) {
	s, _ := fieldProjector(t).Parse("*")
	for _, data := range []string{`null`, `[]`, `{}`, `{"items":null}`, `{"items":{}}`, `{"items":[null]}`, `{"items":[[]]}`, `{"items":[1]}`, `{"items":[],"items":[]}`, `{"items":[{"id":"a","id":"b"}]}`, `{"items":[{"profile":false}]}`, `{"items":[{"name":"\ud800"}]}`, `{"items":[] } {}`, `{"items":[` + strings.Repeat(`{},`, 1000) + `{}]}`, `{"items":[{"tags":[` + strings.Repeat(`0,`, 1000) + `0]}]}`} {
		if _, err := s.ProjectList(json.RawMessage(data), "items"); !errors.Is(err, transport.ErrProjection) {
			t.Fatalf("bad response accepted: %.100s: %v", data, err)
		}
	}
	if _, err := s.ProjectList(map[string]any{"items": []any{map[string]any{"name": strings.Repeat("x", transport.MaxResponseBytes)}}}, "items"); !errors.Is(err, transport.ErrProjection) {
		t.Fatal("response bound", err)
	}
	var zero transport.FieldSelection
	if _, err := zero.ProjectList(map[string]any{"items": []any{}}, "items"); !errors.Is(err, transport.ErrProjection) {
		t.Fatal("zero selection accepted", err)
	}
}

func TestProjectionSchemaBoundsAndCopy(t *testing.T) {
	cycle := transport.FieldSchema{}
	cycle["next"] = cycle
	wide := transport.FieldSchema{}
	for i := 0; i < 1025; i++ {
		wide[fmt.Sprint("field", i)] = nil
	}
	for _, schema := range []transport.FieldSchema{nil, {}, {"bad.path": nil}, {"name": {}}, cycle, wide} {
		if _, err := transport.NewFieldProjector(schema); !errors.Is(err, transport.ErrProjection) {
			t.Fatal("invalid schema", err)
		}
	}
	child := transport.FieldSchema{"name": nil}
	schema := transport.FieldSchema{"profile": child}
	p, err := transport.NewFieldProjector(schema)
	if err != nil {
		t.Fatal(err)
	}
	child["secret"] = nil
	schema["secret"] = nil
	if _, err := p.Parse("profile.secret"); !errors.Is(err, transport.ErrRequest) {
		t.Fatal("schema changed after construction", err)
	}
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			for range 10 {
				selection, err := p.Parse("profile.*")
				if err != nil {
					t.Error(err)
					return
				}
				result, err := selection.ProjectList(json.RawMessage(`{"items":[{"profile":{"name":"A","secret":"hidden"}}]}`), "items")
				if err != nil || strings.Contains(string(result), "hidden") {
					t.Error("concurrent projection failed", err)
				}
			}
		})
	}
	wg.Wait()
}

func TestProjectionBoundsAllResponseData(t *testing.T) {
	selection, err := fieldProjector(t).Parse("id")
	if err != nil {
		t.Fatal(err)
	}
	wide := map[string]any{}
	for i := range 1025 {
		wide[fmt.Sprint("field", i)] = i
	}
	row := map[string]any{}
	for i := range 100 {
		row[fmt.Sprint("field", i)] = i
	}
	large := make([]any, 1000)
	for i := range large {
		large[i] = row
	}
	for _, value := range []any{
		map[string]any{"items": []any{wide}},
		map[string]any{"items": large},
		json.RawMessage(`{"items":[{"ignored":` + strings.Repeat("[", 33) + "0" + strings.Repeat("]", 33) + `}]}`),
	} {
		if _, err := selection.ProjectList(value, "items"); !errors.Is(err, transport.ErrProjection) {
			t.Fatal("unselected data exceeded its bound", err)
		}
	}
}

func BenchmarkProjectionPage(b *testing.B) {
	p := fieldProjector(b)
	selection, err := p.Parse("id,name")
	if err != nil {
		b.Fatal(err)
	}
	items := make([]map[string]any, 100)
	for i := range items {
		items[i] = map[string]any{"id": fmt.Sprint(i), "name": "record", "amount": int64(9007199254740993), "profile": map[string]any{"name": "owner", "active": true}}
	}
	page := map[string]any{"items": items, "total": 100, "page": 1, "size": 100}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := selection.ProjectList(page, "items"); err != nil {
			b.Fatal(err)
		}
	}
}
