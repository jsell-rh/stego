package httpapplication

import (
	"reflect"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/types"
)

func nullableResponseContext() gen.Context {
	c := responseContext()
	m := c.ComponentConfig["response_mappings"].([]any)[0].(map[string]any)
	fields := m["fields"].([]any)
	fields[12] = map[string]any{"target": "nullable_note", "source": "note", "on_absent": "emit_null"}
	schema := strings.Replace(responseFixture, "required: [title, count]", "required: [title, count, nullable_required]", 1)
	schema += `            nullable_omit: {type: string, nullable: true}
            nullable_required: {type: string, nullable: true}
            nullable_enabled: {type: boolean, nullable: true}
            nullable_count: {type: integer, format: int32, nullable: true}
            nullable_small: {type: integer, format: int32, nullable: true}
            nullable_large: {type: integer, format: int64, nullable: true}
            nullable_ratio: {type: number, format: double, nullable: true}
            nullable_decimal: {type: number, format: float, nullable: true}
            nullable_recorded: {type: string, format: date-time, nullable: true}
            nullable_value: {type: string, nullable: true}
            nullable_constant: {type: string, nullable: true}
            nullable_input: {type: string, nullable: true}
`
	c.Inputs["api.yaml"] = []byte(schema)
	for _, item := range []struct{ target, source, policy string }{
		{"nullable_omit", "note", "omit"}, {"nullable_required", "note", "emit_null"},
		{"nullable_enabled", "active", "emit_null"}, {"nullable_count", "limit", "emit_null"},
		{"nullable_small", "small", "emit_null"}, {"nullable_large", "large", "emit_null"},
		{"nullable_ratio", "ratio", "emit_null"}, {"nullable_decimal", "decimal", "emit_null"},
		{"nullable_recorded", "recorded", "emit_null"},
	} {
		f := map[string]any{"target": item.target, "source": item.source, "on_absent": item.policy}
		if item.target == "nullable_count" {
			f["conversion"] = "int32"
		}
		fields = append(fields, f)
	}
	fields = append(fields, map[string]any{"target": "nullable_value", "source": "title"},
		map[string]any{"target": "nullable_constant", "constant": "fixed"},
		map[string]any{"target": "nullable_input", "input": "Display", "on_absent": "emit_null"})
	m["fields"] = fields
	m["inputs"] = []any{map[string]any{"name": "Display", "type": "string", "optional": true}}
	source := c.GoModelSources["custom-store"]
	for _, item := range []struct {
		name, selection string
		kind            types.FieldType
	}{
		{"recorded", "Recorded", types.FieldTypeTimestamp}, {"limit", "Limit", types.FieldTypeInt64},
		{"small", "Small", types.FieldTypeInt32}, {"large", "Large", types.FieldTypeInt64},
		{"ratio", "Ratio", types.FieldTypeDouble}, {"decimal", "Decimal", types.FieldTypeFloat},
	} {
		source.Models[0].Fields = append(source.Models[0].Fields, gen.GoModelField{Name: item.name, Selection: item.selection, Type: item.kind, Pointer: true})
	}
	c.GoModelSources["custom-store"] = source
	return c
}

func nullableRule(c gen.Context, target string) map[string]any {
	for _, f := range c.ComponentConfig["response_mappings"].([]any)[0].(map[string]any)["fields"].([]any) {
		if rule := f.(map[string]any); rule["target"] == target {
			return rule
		}
	}
	panic("missing test rule")
}

func TestHTTPResponseNullableValidation(t *testing.T) {
	for name, change := range map[string]func(gen.Context){
		"nullable enum": func(c gen.Context) {
			c.Inputs["api.yaml"] = []byte(strings.Replace(string(c.Inputs["api.yaml"]), "nullable_note: {type: string, nullable: true}", "nullable_note: {type: string, nullable: true, enum: [ready]}", 1))
		},
		"nullable integer enum": func(c gen.Context) {
			c.Inputs["api.yaml"] = []byte(strings.Replace(string(c.Inputs["api.yaml"]), "nullable_small: {type: integer, format: int32, nullable: true}", "nullable_small: {type: integer, format: int32, nullable: true, enum: [1, 2]}", 1))
		},
		"missing policy":       func(c gen.Context) { delete(nullableRule(c, "nullable_note"), "on_absent") },
		"unknown policy":       func(c gen.Context) { nullableRule(c, "nullable_note")["on_absent"] = "guess" },
		"null policy":          func(c gen.Context) { nullableRule(c, "nullable_note")["on_absent"] = nil },
		"boolean policy":       func(c gen.Context) { nullableRule(c, "nullable_note")["on_absent"] = true },
		"required omission":    func(c gen.Context) { nullableRule(c, "nullable_required")["on_absent"] = "omit" },
		"non-null target":      func(c gen.Context) { nullableRule(c, "note")["on_absent"] = "emit_null" },
		"value source":         func(c gen.Context) { nullableRule(c, "nullable_value")["on_absent"] = "emit_null" },
		"constant policy":      func(c gen.Context) { nullableRule(c, "nullable_constant")["on_absent"] = "emit_null" },
		"empty omission":       func(c gen.Context) { nullableRule(c, "nullable_note")["omit_empty"] = true },
		"pointer prefix":       func(c gen.Context) { nullableRule(c, "nullable_note")["prefix"] = "prefix" },
		"unchecked narrowing":  func(c gen.Context) { delete(nullableRule(c, "nullable_count"), "conversion") },
		"missing input policy": func(c gen.Context) { delete(nullableRule(c, "nullable_input"), "on_absent") },
		"nullable object": func(c gen.Context) {
			c.Inputs["api.yaml"] = []byte(strings.Replace(string(c.Inputs["api.yaml"]), "nullable_note: {type: string, nullable: true}", "nullable_note: {type: object, nullable: true}", 1))
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := nullableResponseContext()
			change(c)
			g := new(Generator)
			if err := g.ValidateContext(c); err == nil {
				t.Fatal("invalid nullable mapping passed validation")
			}
			if files, wiring, err := g.Generate(c); err == nil || files != nil || wiring != nil {
				t.Fatal("invalid nullable mapping produced output")
			}
		})
	}
}

func TestHTTPResponseNullableDeterminism(t *testing.T) {
	c := nullableResponseContext()
	g := new(Generator)
	a, aw, err := g.Generate(c)
	if err != nil {
		t.Fatal(err)
	}
	b, bw, err := g.Generate(c)
	if err != nil || !reflect.DeepEqual(a, b) || !reflect.DeepEqual(aw, bw) {
		t.Fatal("nullable output changed on repeat", err)
	}
}

func TestGeneratedHTTPResponseNullable(t *testing.T) {
	checkGeneratedResponses(t, nullableResponseContext(), nullableResponseRuntime, []string{"", "TestNullablePresence", "TestNullableOwnsValues", "TestNullableRejectsInvalidValues", "TestNullableNumericBounds"})
}

const nullableResponseRuntime = `package responses
import("encoding/json";"math";"testing";"time";"example.com/dispatch/out/store")
func jsonObject(t *testing.T,v any) map[string]json.RawMessage {t.Helper();b,e:=json.Marshal(v);if e!=nil{t.Fatal(e)};var result map[string]json.RawMessage;if e=json.Unmarshal(b,&result);e!=nil{t.Fatal(e)};return result}
func nullableRow() store.Record {return store.Record{CreatedTime:time.Date(2026,9,21,1,2,3,0,time.UTC)}}
func TestNullablePresence(t *testing.T){
 r,e:=Shipment(nullableRow(),ShipmentInput{});if e!=nil{t.Fatal(e)};got:=jsonObject(t,r)
 for _,name:=range []string{"nullable_note","nullable_required","nullable_enabled","nullable_count","nullable_small","nullable_large","nullable_ratio","nullable_decimal","nullable_recorded","nullable_input"}{if string(got[name])!="null"{t.Fatal("missing explicit null",name,string(got[name]))}}
 if _,ok:=got["nullable_omit"];ok{t.Fatal("absent value was not omitted")}
 if string(got["nullable_value"])!="\"\""||string(got["nullable_constant"])!="\"fixed\""{t.Fatal("value or constant presence changed")}
 row:=nullableRow();empty:="";no:=false;zero:=int64(0);small:=int32(0);ratio:=float64(0);decimal:=float32(0);stamp:=time.Time{}
 row.Note=&empty;row.Active=&no;row.Limit=&zero;row.Small=&small;row.Large=&zero;row.Ratio=&ratio;row.Decimal=&decimal;row.Recorded=&stamp
 r,e=Shipment(row,ShipmentInput{Display:&empty});if e!=nil{t.Fatal(e)};got=jsonObject(t,r)
 for _,name:=range []string{"nullable_note","nullable_required","nullable_omit","nullable_input"}{if string(got[name])!="\"\""{t.Fatal("present empty string changed",name)}}
 for _,name:=range []string{"nullable_count","nullable_small","nullable_large","nullable_ratio","nullable_decimal"}{if string(got[name])!="0"{t.Fatal("present zero changed",name)}}
 if string(got["nullable_enabled"])!="false"||string(got["nullable_recorded"])!="\"0001-01-01T00:00:00Z\""{t.Fatal("present scalar changed")}
}
func TestNullableOwnsValues(t *testing.T){
 row:=nullableRow();note:="before";row.Note=&note;display:="display";r,e:=Shipment(row,ShipmentInput{Display:&display});if e!=nil{t.Fatal(e)}
 note="after";display="changed";got:=jsonObject(t,r);if string(got["nullable_note"])!="\"before\""||string(got["nullable_input"])!="\"display\""{t.Fatal("response shares source values")}
 r.NullableNote.SetNull();got=jsonObject(t,r);if string(got["nullable_required"])!="\"before\""||string(got["nullable_omit"])!="\"before\""{t.Fatal("response fields share nullable storage")}
}
func TestNullableRejectsInvalidValues(t *testing.T){
 for name,mutate:=range map[string]func(*store.Record,*ShipmentInput){
 "text":func(r *store.Record,i *ShipmentInput){s:=string([]byte{255});r.Note=&s},
 "input":func(r *store.Record,i *ShipmentInput){s:=string([]byte{255});i.Display=&s},
 "narrow":func(r *store.Record,i *ShipmentInput){v:=int64(math.MaxInt32)+1;r.Limit=&v},
 "narrow low":func(r *store.Record,i *ShipmentInput){v:=int64(math.MinInt32)-1;r.Limit=&v},
 "nan":func(r *store.Record,i *ShipmentInput){v:=math.NaN();r.Ratio=&v},
 "infinity":func(r *store.Record,i *ShipmentInput){v:=float32(math.Inf(1));r.Decimal=&v},
 "timestamp":func(r *store.Record,i *ShipmentInput){v:=time.Date(10000,1,1,0,0,0,0,time.UTC);r.Recorded=&v},
 "offset":func(r *store.Record,i *ShipmentInput){v:=r.CreatedTime.In(time.FixedZone("seconds",30));r.Recorded=&v},
 }{t.Run(name,func(t *testing.T){row:=nullableRow();input:=ShipmentInput{};mutate(&row,&input);got,e:=Shipment(row,input);if got!=nil||e!=ErrConversion||e.Error()!="response conversion failed"{t.Fatal("invalid value returned output or changed the private error",e)}})}
}
func TestNullableNumericBounds(t *testing.T){
 for _,bound:=range []int64{math.MinInt32,math.MaxInt32}{row:=nullableRow();row.Limit=&bound;small:=int32(bound);row.Small=&small;large:=int64(math.MaxInt64);row.Large=&large;r,e:=Shipment(row,ShipmentInput{});if e!=nil{t.Fatal(e)};got:=jsonObject(t,r);if string(got["nullable_large"])!="9223372036854775807"{t.Fatal("integer precision changed")}}
}
`
