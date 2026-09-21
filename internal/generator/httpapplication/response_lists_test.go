package httpapplication

import (
	"reflect"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/types"
)

func responseJSONContext() gen.Context {
	c := responseContext()
	source := strings.Replace(responseFixture, "required: [title, count]", "required: [title, count, required_tags]", 1)
	source += "            tags: {type: array, items: {type: string}}\n            required_tags: {type: array, items: {type: string}}\n"
	c.Inputs["api.yaml"] = []byte(source)
	provider := c.GoModelSources["custom-store"]
	provider.Models[0].Fields = append(provider.Models[0].Fields, gen.GoModelField{Name: "tags", Selection: "Tags", Type: types.FieldTypeJsonb})
	c.GoModelSources["custom-store"] = provider
	m := c.ComponentConfig["response_mappings"].([]any)[0].(map[string]any)
	fields := m["fields"].([]any)
	rule := func(target string) map[string]any {
		return map[string]any{"target": target, "source": "tags", "conversion": "json_strings", "max_bytes": 64, "max_items": 3, "max_item_bytes": 16}
	}
	omitted := rule("ignored")
	omitted["omit_empty"] = true
	fields[11] = omitted
	m["fields"] = append(fields, rule("tags"), rule("required_tags"))
	return c
}

func TestHTTPResponseJSONMappingValidation(t *testing.T) {
	mapping := func(c gen.Context) map[string]any {
		return c.ComponentConfig["response_mappings"].([]any)[0].(map[string]any)
	}
	rule := func(c gen.Context) map[string]any { return mapping(c)["fields"].([]any)[13].(map[string]any) }
	for name, edit := range map[string]func(gen.Context){
		"missing bound":     func(c gen.Context) { delete(rule(c), "max_bytes") },
		"zero bytes":        func(c gen.Context) { rule(c)["max_bytes"] = 0 },
		"excess bytes":      func(c gen.Context) { rule(c)["max_bytes"] = 16<<20 + 1 },
		"zero items":        func(c gen.Context) { rule(c)["max_items"] = 0 },
		"excess items":      func(c gen.Context) { rule(c)["max_items"] = 65537 },
		"zero item bytes":   func(c gen.Context) { rule(c)["max_item_bytes"] = 0 },
		"excess item bytes": func(c gen.Context) { rule(c)["max_item_bytes"] = 65 },
		"non integer limit": func(c gen.Context) { rule(c)["max_bytes"] = 64.0 },
		"boolean limit":     func(c gen.Context) { rule(c)["max_items"] = true },
		"no conversion":     func(c gen.Context) { delete(rule(c), "conversion") },
		"scalar limit":      func(c gen.Context) { mapping(c)["fields"].([]any)[0].(map[string]any)["max_bytes"] = 64 },
		"prefix":            func(c gen.Context) { rule(c)["prefix"] = "/" },
		"constant":          func(c gen.Context) { rule(c)["constant"] = "[]" },
		"false omission":    func(c gen.Context) { rule(c)["omit_empty"] = false },
		"required omission": func(c gen.Context) { mapping(c)["fields"].([]any)[14].(map[string]any)["omit_empty"] = true },
		"non JSON source":   func(c gen.Context) { rule(c)["source"] = "title" },
		"pointer source":    func(c gen.Context) { c.GoModelSources["custom-store"].Models[0].Fields[9].Pointer = true },
		"nullable items": func(c gen.Context) {
			c.Inputs["api.yaml"] = []byte(strings.ReplaceAll(string(c.Inputs["api.yaml"]), "items: {type: string}", "items: {type: string, nullable: true}"))
		},
		"numeric items": func(c gen.Context) {
			c.Inputs["api.yaml"] = []byte(strings.ReplaceAll(string(c.Inputs["api.yaml"]), "items: {type: string}", "items: {type: integer}"))
		},
		"nullable array": func(c gen.Context) {
			c.Inputs["api.yaml"] = []byte(strings.ReplaceAll(string(c.Inputs["api.yaml"]), "type: array, items:", "type: array, nullable: true, items:"))
		},
		"combined bytes": func(c gen.Context) { rule(c)["max_bytes"] = 16 << 20 },
		"combined items": func(c gen.Context) { rule(c)["max_items"] = 65536 },
	} {
		t.Run(name, func(t *testing.T) {
			c := responseJSONContext()
			edit(c)
			g := new(Generator)
			if err := g.ValidateContext(c); err == nil {
				t.Fatal("invalid list mapping passed validation")
			}
			if files, wiring, err := g.Generate(c); err == nil || files != nil || wiring != nil {
				t.Fatal("invalid list mapping returned output")
			}
		})
	}
}

func TestHTTPResponseJSONMappingDeterminism(t *testing.T) {
	c := responseJSONContext()
	g := new(Generator)
	if err := g.ValidateContext(c); err != nil {
		t.Fatal(err)
	}
	files, wiring, err := g.Generate(c)
	if err != nil {
		t.Fatal(err)
	}
	again, other, err := g.Generate(c)
	if err != nil || !reflect.DeepEqual(files, again) || !reflect.DeepEqual(wiring, other) {
		t.Fatal("list output changed on repeat", err)
	}
	count := 0
	for _, file := range files {
		if strings.HasSuffix(file.Path, "/responses/json_strings.go") {
			count++
		}
	}
	if count != 1 {
		t.Fatal("list mappings need exactly one shared decoder")
	}
	scalar, _, err := g.Generate(responseContext())
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range scalar {
		if strings.HasSuffix(file.Path, "/responses/json_strings.go") {
			t.Fatal("unused decoder was generated")
		}
	}
}

func TestHTTPResponseJSONMappingBounds(t *testing.T) {
	c := responseJSONContext()
	fields := c.ComponentConfig["response_mappings"].([]any)[0].(map[string]any)["fields"].([]any)
	rule := fields[13].(map[string]any)
	rule["max_bytes"] = (16 << 20) - 128
	rule["max_items"] = 65530
	if err := new(Generator).ValidateContext(c); err != nil {
		t.Fatal("exact aggregate JSON limits were rejected", err)
	}
}

func TestGeneratedHTTPResponseJSONMappings(t *testing.T) {
	checkGeneratedResponses(t, responseJSONContext(), responseJSONRuntimeTest, []string{"", "TestResponseListsPresence", "TestResponseListsRejectInvalidInput", "TestResponseListsOwnOutput"})
}

const responseJSONRuntimeTest = `package responses
import("bytes";"encoding/json";"reflect";"strings";"testing";"example.com/dispatch/out/store")
func jsonValue(t *testing.T,v any) map[string]json.RawMessage {t.Helper();raw,e:=json.Marshal(v);if e!=nil{t.Fatal(e)};var result map[string]json.RawMessage;if e=json.Unmarshal(raw,&result);e!=nil{t.Fatal(e)};return result}
func TestResponseListsPresence(t *testing.T){
 for _,raw:=range []string{"","null","[]","[]"+strings.Repeat(" ",62)}{
  r,e:=Shipment(store.Record{Tags:[]byte(raw)});if e!=nil{t.Fatal(e)};got:=jsonValue(t,r)
  if _,ok:=got["ignored"];ok{t.Fatal("empty list was not omitted")}
  for _,name:=range []string{"tags","required_tags"}{if string(got[name])!="[]"{t.Fatal("empty array became absent or null",name)}}
 }
 for _,raw:=range []string{"[\"\"]","[\"one\",\"two\",\"three\"]","[\"1234567890123456\"]","[\"\\ud83d\\ude80\",\"é\",\"\\\\ud800\"]"}{
  r,e:=Shipment(store.Record{Tags:[]byte(raw)});if e!=nil{t.Fatal(e)};got:=jsonValue(t,r);var want []string;if e=json.Unmarshal([]byte(raw),&want);e!=nil{t.Fatal(e)}
  for _,name:=range []string{"ignored","tags","required_tags"}{var actual []string;if e=json.Unmarshal(got[name],&actual);e!=nil||!reflect.DeepEqual(actual,want){t.Fatal("list values changed",name,e)}}
 }
}
func TestResponseListsRejectInvalidInput(t *testing.T){
 bad:=[][]byte{[]byte("[\"one\",\"two\",\"three\",\"four\"]"),[]byte("[\"12345678901234567\"]"),[]byte("[]"+strings.Repeat(" ",63)),[]byte("[1]"),[]byte("[null]"),[]byte("[[]]"),[]byte("{}"),[]byte("\"text\""),[]byte("[] []"),[]byte("null 1"),[]byte("[\"x\",]"),[]byte("[\"\\ud800\"]"),[]byte("[\"\\udc00\"]"),[]byte("[\"\\ud800\\u0041\"]"),[]byte{'[','"',255,'"',']'}}
 for i,raw:=range bad{r,e:=Shipment(store.Record{Tags:raw});if r!=nil||e!=ErrConversion||e.Error()!="response conversion failed"{t.Fatal("invalid list returned data or variable error",i)}}
}
func TestResponseListsOwnOutput(t *testing.T){
 raw:=[]byte("[\"first\",\"second\"]");before:=bytes.Clone(raw);r,e:=Shipment(store.Record{Tags:raw});if e!=nil{t.Fatal(e)}
 (*r.Tags)[0]="changed";if (*r.Ignored)[0]!="first"||r.RequiredTags[0]!="first"||!bytes.Equal(raw,before){t.Fatal("response fields or source share list storage")}
 for i:=range raw{raw[i]='x'};if (*r.Tags)[1]!="second"||(*r.Ignored)[1]!="second"{t.Fatal("source mutation changed response")}
}
`
