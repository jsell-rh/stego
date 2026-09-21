package httpapplication

import (
	"reflect"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
)

func enumResponseContext() gen.Context {
	c := responseContext()
	fixture := responseFixture
	for old, new := range map[string]string{
		"title: {type: string}":   "title: {type: string, enum: [ready, sent]}",
		"kind: {type: string}":    "kind: {type: string, enum: [Shipment]}",
		"href: {type: string}":    "href: {type: string, enum: ['/shipments/one']}",
		"note: {type: string}":    "note: {type: string, enum: ['', ready]}",
		"creator: {type: string}": "creator: {type: string, enum: [ready]}",
	} {
		fixture = strings.Replace(fixture, old, new, 1)
	}
	c.Inputs["api.yaml"] = []byte(fixture)
	m := c.ComponentConfig["response_mappings"].([]any)[0].(map[string]any)
	m["inputs"] = []any{map[string]any{"name": "Title", "type": "string"}}
	rule := m["fields"].([]any)[4].(map[string]any)
	delete(rule, "source")
	rule["input"] = "Title"
	return c
}

func TestHTTPResponseEnumValidation(t *testing.T) {
	for name, edit := range map[string]func(gen.Context){
		"constant outside enum": func(c gen.Context) {
			m := c.ComponentConfig["response_mappings"].([]any)[0].(map[string]any)
			m["fields"].([]any)[1].(map[string]any)["constant"] = "Unknown"
		},
		"duplicate value": func(c gen.Context) {
			c.Inputs["api.yaml"] = []byte(strings.Replace(string(c.Inputs["api.yaml"]), "[ready, sent]", "[ready, ready]", 1))
		},
		"wrong source type": func(c gen.Context) {
			m := c.ComponentConfig["response_mappings"].([]any)[0].(map[string]any)
			m["inputs"].([]any)[0].(map[string]any)["type"] = "bool"
		},
		"required omission": func(c gen.Context) {
			m := c.ComponentConfig["response_mappings"].([]any)[0].(map[string]any)
			m["fields"].([]any)[4].(map[string]any)["omit_empty"] = true
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := enumResponseContext()
			edit(c)
			g := new(Generator)
			if err := g.ValidateContext(c); err == nil {
				t.Fatal("invalid enum mapping passed validation")
			}
			if files, wiring, err := g.Generate(c); err == nil || files != nil || wiring != nil {
				t.Fatal("invalid enum mapping returned output")
			}
		})
	}
	c := enumResponseContext()
	g := new(Generator)
	files, wiring, err := g.Generate(c)
	if err != nil {
		t.Fatal(err)
	}
	again, other, err := g.Generate(c)
	if err != nil || !reflect.DeepEqual(files, again) || !reflect.DeepEqual(wiring, other) {
		t.Fatal("enum output changed on repeat", err)
	}
}

func TestGeneratedHTTPResponseEnums(t *testing.T) {
	checkGeneratedResponses(t, enumResponseContext(), responseEnumRuntimeTest, []string{"", "TestEnumValuesAndPresence", "TestEnumOwnership", "TestEnumRejectsUnknownValues"})
}

const responseEnumRuntimeTest = `package responses
import("encoding/json";"testing";"example.com/dispatch/out/store")
func enumRow()store.Record{return store.Record{ID:"one"}}
func TestEnumValuesAndPresence(t *testing.T){
 r,e:=Shipment(enumRow(),ShipmentInput{Title:"ready"});if e!=nil{t.Fatal(e)}
 if string(r.Title)!="ready"||string(*r.Kind)!="Shipment"||string(*r.Href)!="/shipments/one"||r.Note!=nil||r.Creator!=nil{t.Fatal("enum values or absence changed")}
 empty:="";row:=enumRow();row.Note=&empty;row.Creator="ready"
 r,e=Shipment(row,ShipmentInput{Title:"sent"});if e!=nil{t.Fatal(e)}
 raw,e:=json.Marshal(r);if e!=nil{t.Fatal(e)};var got map[string]json.RawMessage;if e=json.Unmarshal(raw,&got);e!=nil{t.Fatal(e)}
 for k,w:=range map[string]string{"title":"\"sent\"","note":"\"\"","creator":"\"ready\""}{if string(got[k])!=w{t.Fatal("enum wire value changed",k)}}
}
func TestEnumOwnership(t *testing.T){
 note:="ready";row:=enumRow();row.Note=&note;r,e:=Shipment(row,ShipmentInput{Title:"ready"});if e!=nil{t.Fatal(e)}
 *r.Note="";if note!="ready"{t.Fatal("response changed source")};note="later";if string(*r.Note)!=""{t.Fatal("source changed response")}
}
func TestEnumRejectsUnknownValues(t *testing.T){
 for name,edit:=range map[string]func(*store.Record,*ShipmentInput){
 "prepared":func(r *store.Record,i *ShipmentInput){i.Title="unknown"},
 "required empty":func(r *store.Record,i *ShipmentInput){i.Title=""},
 "pointer":func(r *store.Record,i *ShipmentInput){v:="unknown";r.Note=&v},
 "stored":func(r *store.Record,i *ShipmentInput){r.Creator="unknown"},
 "prefixed":func(r *store.Record,i *ShipmentInput){r.ID="two"},
 "encoding":func(r *store.Record,i *ShipmentInput){i.Title=string([]byte{255})},
 }{t.Run(name,func(t *testing.T){row:=enumRow();input:=ShipmentInput{Title:"ready"};edit(&row,&input);r,e:=Shipment(row,input);if r!=nil||e!=ErrConversion||e.Error()!="response conversion failed"{t.Fatal("invalid enum returned data or variable error")}})}
}
`
