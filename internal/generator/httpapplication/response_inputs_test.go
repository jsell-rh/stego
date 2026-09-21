package httpapplication

import (
	"reflect"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
)

func preparedResponseContext() gen.Context {
	c := responseJSONContext()
	m := c.ComponentConfig["response_mappings"].([]any)[0].(map[string]any)
	m["inputs"] = []any{
		map[string]any{"name": "Creator", "type": "string"},
		map[string]any{"name": "Note", "type": "string", "optional": true},
		map[string]any{"name": "Created", "type": "timestamp"},
		map[string]any{"name": "Count", "type": "int64"},
		map[string]any{"name": "Tags", "type": "jsonb"},
	}
	fields := m["fields"].([]any)
	for index, name := range map[int]string{3: "Created", 5: "Count", 6: "Note", 9: "Creator", 11: "Tags", 13: "Tags", 14: "Tags"} {
		rule := fields[index].(map[string]any)
		delete(rule, "source")
		rule["input"] = name
	}
	return c
}

func TestHTTPResponsePreparedInputValidation(t *testing.T) {
	mapping := func(c gen.Context) map[string]any {
		return c.ComponentConfig["response_mappings"].([]any)[0].(map[string]any)
	}
	field := func(c gen.Context, index int) map[string]any {
		return mapping(c)["fields"].([]any)[index].(map[string]any)
	}
	input := func(c gen.Context, index int) map[string]any {
		return mapping(c)["inputs"].([]any)[index].(map[string]any)
	}
	for name, edit := range map[string]func(gen.Context){
		"empty inputs":           func(c gen.Context) { mapping(c)["inputs"] = []any{} },
		"non list inputs":        func(c gen.Context) { mapping(c)["inputs"] = "Creator" },
		"too many inputs":        func(c gen.Context) { mapping(c)["inputs"] = make([]any, 129) },
		"non object input":       func(c gen.Context) { mapping(c)["inputs"].([]any)[0] = "Creator" },
		"missing name":           func(c gen.Context) { delete(input(c, 0), "name") },
		"unknown input option":   func(c gen.Context) { input(c, 0)["expression"] = "lookup()" },
		"unknown mapping option": func(c gen.Context) { mapping(c)["lookup"] = "Creator" },
		"private input":          func(c gen.Context) { input(c, 0)["name"] = "creator" },
		"expression name":        func(c gen.Context) { input(c, 0)["name"] = "Creator()" },
		"duplicate input":        func(c gen.Context) { input(c, 1)["name"] = "Creator" },
		"arbitrary type":         func(c gen.Context) { input(c, 0)["type"] = "domain.Creator" },
		"invalid presence":       func(c gen.Context) { input(c, 0)["optional"] = "true" },
		"JSON pointer":           func(c gen.Context) { input(c, 4)["optional"] = true },
		"unused input": func(c gen.Context) {
			mapping(c)["inputs"] = append(mapping(c)["inputs"].([]any), map[string]any{"name": "Unused", "type": "string"})
		},
		"unknown input":           func(c gen.Context) { field(c, 9)["input"] = "Absent" },
		"non string selector":     func(c gen.Context) { field(c, 9)["input"] = true },
		"both sources":            func(c gen.Context) { field(c, 9)["source"] = "creator" },
		"input and constant":      func(c gen.Context) { field(c, 9)["constant"] = "someone" },
		"input and omission":      func(c gen.Context) { field(c, 9)["omit"] = true },
		"wrong type":              func(c gen.Context) { input(c, 0)["type"] = "bool" },
		"optional required input": func(c gen.Context) { input(c, 3)["optional"] = true },
		"name conflict": func(c gen.Context) {
			other := responseJSONContext().ComponentConfig["response_mappings"].([]any)[0].(map[string]any)
			other["name"] = "ShipmentInput"
			c.ComponentConfig["response_mappings"] = []any{mapping(c), other}
		},
		"reverse name conflict": func(c gen.Context) {
			other := responseJSONContext().ComponentConfig["response_mappings"].([]any)[0].(map[string]any)
			other["name"] = "ShipmentInput"
			c.ComponentConfig["response_mappings"] = []any{other, mapping(c)}
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := preparedResponseContext()
			edit(c)
			g := new(Generator)
			if err := g.ValidateContext(c); err == nil {
				t.Fatal("invalid prepared input passed validation")
			}
			if files, wiring, err := g.Generate(c); err == nil || files != nil || wiring != nil {
				t.Fatal("invalid prepared input returned output")
			}
		})
	}
}

func TestHTTPResponsePreparedInputDeterminism(t *testing.T) {
	c := preparedResponseContext()
	g := new(Generator)
	if err := g.ValidateContext(c); err != nil {
		t.Fatal(err)
	}
	files, wiring, err := g.Generate(c)
	if err != nil {
		t.Fatal(err)
	}
	m := c.ComponentConfig["response_mappings"].([]any)[0].(map[string]any)
	inputs := m["inputs"].([]any)
	for i, j := 0, len(inputs)-1; i < j; i, j = i+1, j-1 {
		inputs[i], inputs[j] = inputs[j], inputs[i]
	}
	again, other, err := g.Generate(c)
	if err != nil || !reflect.DeepEqual(files, again) || !reflect.DeepEqual(wiring, other) {
		t.Fatal("input declaration order changed generated output", err)
	}
	for _, raw := range m["fields"].([]any) {
		rule := raw.(map[string]any)
		if _, ok := rule["input"]; ok {
			if _, changed := rule["source"]; changed {
				t.Fatal("generation mutated the input declaration")
			}
		}
	}
}

func TestGeneratedHTTPResponsePreparedInputs(t *testing.T) {
	checkGeneratedResponses(t, preparedResponseContext(), responseInputRuntimeTest, []string{"", "TestPreparedInputsUseDomainValues", "TestPreparedInputsOwnOutput", "TestPreparedInputsRejectInvalidValues"})
}

const responseInputRuntimeTest = `package responses
import("encoding/json";"testing";"time";"example.com/dispatch/out/store")
func prepared() ShipmentInput {return ShipmentInput{Creator:"resolved-person",Created:time.Date(2026,9,21,1,2,3,4,time.UTC),Tags:[]byte("[\"domain\"]")}}
func object(t *testing.T,v any)map[string]json.RawMessage{t.Helper();raw,e:=json.Marshal(v);if e!=nil{t.Fatal(e)};var got map[string]json.RawMessage;if e=json.Unmarshal(raw,&got);e!=nil{t.Fatal(e)};return got}
func TestPreparedInputsUseDomainValues(t *testing.T){
 row:=store.Record{Creator:"stored-person",Count:99,Tags:[]byte("not JSON")}
 input:=prepared();r,e:=Shipment(row,input);if e!=nil{t.Fatal(e)};got:=object(t,r)
 for k,want:=range map[string]string{"creator":"\"resolved-person\"","count":"0","created":"\"2026-09-21T01:02:03.000000004Z\"","tags":"[\"domain\"]","required_tags":"[\"domain\"]"}{if string(got[k])!=want{t.Fatal("wrong prepared value",k,string(got[k]))}}
 if _,ok:=got["note"];ok{t.Fatal("absent input pointer was emitted")}
 input.Creator="";input.Tags=nil;r,e=Shipment(row,input);if e!=nil{t.Fatal(e)};got=object(t,r)
 if _,ok:=got["creator"];ok{t.Fatal("empty prepared creator was not omitted")}
 if _,ok:=got["ignored"];ok{t.Fatal("empty prepared list was not omitted")}
 if string(got["tags"])!="[]"||string(got["required_tags"])!="[]"{t.Fatal("empty prepared arrays changed presence")}
}
func TestPreparedInputsOwnOutput(t *testing.T){
 input:=prepared();note:="";input.Note=&note;r,e:=Shipment(store.Record{},input);if e!=nil{t.Fatal(e)}
 if string(object(t,r)["note"])!="\"\""{t.Fatal("present empty input was lost")}
 *r.Note="response";(*r.Tags)[0]="response";if note!=""||string(input.Tags)!="[\"domain\"]"||r.RequiredTags[0]!="domain"{t.Fatal("response shares prepared storage")}
 note="later";for i:=range input.Tags{input.Tags[i]='x'};if *r.Note!="response"||r.RequiredTags[0]!="domain"{t.Fatal("prepared mutation changed response")}
}
func TestPreparedInputsRejectInvalidValues(t *testing.T){
 for name,edit:=range map[string]func(*ShipmentInput){
 "encoding":func(v *ShipmentInput){v.Creator=string([]byte{255})},
 "pointer encoding":func(v *ShipmentInput){s:=string([]byte{255});v.Note=&s},
 "timestamp":func(v *ShipmentInput){v.Created=time.Date(10000,1,1,0,0,0,0,time.UTC)},
 "integer":func(v *ShipmentInput){v.Count=2147483648},
 "list":func(v *ShipmentInput){v.Tags=[]byte("[null]")},
 }{t.Run(name,func(t *testing.T){input:=prepared();edit(&input);r,e:=Shipment(store.Record{},input);if r!=nil||e!=ErrConversion||e.Error()!="response conversion failed"{t.Fatal("bad input returned response data or variable error")}})}
}
`
