package httpapplication

import (
	"reflect"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/types"
)

func responseObjectContext() gen.Context {
	c := responseContext()
	c.Inputs["api.yaml"] = []byte(responseFixture + "            metadata: {type: object}\n            copied_metadata: {type: object, additionalProperties: true}\n")
	p := c.GoModelSources["custom-store"]
	p.Models[0].Fields = append(p.Models[0].Fields, gen.GoModelField{Name: "metadata", Selection: "Tags", Type: types.FieldTypeJsonb})
	c.GoModelSources["custom-store"] = p
	m := c.ComponentConfig["response_mappings"].([]any)[0].(map[string]any)
	for _, target := range []string{"metadata", "copied_metadata"} {
		m["fields"] = append(m["fields"].([]any), map[string]any{"target": target, "source": "metadata", "conversion": "json_object",
			"max_bytes": 512, "max_nodes": 32, "max_depth": 4, "max_scalar_bytes": 32, "on_empty": "omit"})
	}
	return c
}

func objectRule(c gen.Context, index int) map[string]any {
	return c.ComponentConfig["response_mappings"].([]any)[0].(map[string]any)["fields"].([]any)[index].(map[string]any)
}

func TestHTTPResponseObjectValidation(t *testing.T) {
	for name, edit := range map[string]func(gen.Context){
		"missing bytes":        func(c gen.Context) { delete(objectRule(c, 13), "max_bytes") },
		"missing nodes":        func(c gen.Context) { delete(objectRule(c, 13), "max_nodes") },
		"missing depth":        func(c gen.Context) { delete(objectRule(c, 13), "max_depth") },
		"missing scalar":       func(c gen.Context) { delete(objectRule(c, 13), "max_scalar_bytes") },
		"missing empty policy": func(c gen.Context) { delete(objectRule(c, 13), "on_empty") },
		"zero bytes":           func(c gen.Context) { objectRule(c, 13)["max_bytes"] = 0 },
		"excess bytes":         func(c gen.Context) { objectRule(c, 13)["max_bytes"] = 16<<20 + 1 },
		"zero nodes":           func(c gen.Context) { objectRule(c, 13)["max_nodes"] = 0 },
		"excess nodes":         func(c gen.Context) { objectRule(c, 13)["max_nodes"] = 65537 },
		"zero depth":           func(c gen.Context) { objectRule(c, 13)["max_depth"] = 0 },
		"excess depth":         func(c gen.Context) { objectRule(c, 13)["max_depth"] = 33 },
		"zero scalar":          func(c gen.Context) { objectRule(c, 13)["max_scalar_bytes"] = 0 },
		"excess scalar":        func(c gen.Context) { objectRule(c, 13)["max_scalar_bytes"] = 513 },
		"float bound":          func(c gen.Context) { objectRule(c, 13)["max_nodes"] = 32.0 },
		"boolean bound":        func(c gen.Context) { objectRule(c, 13)["max_depth"] = true },
		"unknown policy":       func(c gen.Context) { objectRule(c, 13)["on_empty"] = "null" },
		"omit empty object":    func(c gen.Context) { objectRule(c, 13)["omit_empty"] = true },
		"list bound":           func(c gen.Context) { objectRule(c, 13)["max_items"] = 32 },
		"prefix":               func(c gen.Context) { objectRule(c, 13)["prefix"] = "/" },
		"constant":             func(c gen.Context) { objectRule(c, 13)["constant"] = "{}" },
		"scalar source":        func(c gen.Context) { objectRule(c, 13)["source"] = "title" },
		"pointer source":       func(c gen.Context) { c.GoModelSources["custom-store"].Models[0].Fields[9].Pointer = true },
		"absent policy":        func(c gen.Context) { objectRule(c, 13)["on_absent"] = "omit" },
		"scalar target bound":  func(c gen.Context) { objectRule(c, 0)["max_depth"] = 1 },
		"aggregate bytes":      func(c gen.Context) { objectRule(c, 13)["max_bytes"] = 16 << 20 },
		"aggregate nodes":      func(c gen.Context) { objectRule(c, 13)["max_nodes"] = 65536 },
		"required omission": func(c gen.Context) {
			c.Inputs["api.yaml"] = []byte(strings.Replace(string(c.Inputs["api.yaml"]), "required: [title, count]", "required: [title, count, metadata]", 1))
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := responseObjectContext()
			edit(c)
			g := new(Generator)
			if err := g.ValidateContext(c); err == nil {
				t.Fatal("invalid object conversion passed validation")
			}
			if files, wiring, err := g.Generate(c); err == nil || files != nil || wiring != nil {
				t.Fatal("invalid object conversion returned output")
			}
		})
	}
	for name, schema := range map[string]string{
		"nullable":         "{type: object, nullable: true}",
		"closed":           "{type: object, additionalProperties: false}",
		"typed values":     "{type: object, additionalProperties: {type: string}}",
		"fixed properties": "{type: object, properties: {name: {type: string}}}",
		"enum":             "{type: object, enum: [{}]}",
		"min properties":   "{type: object, minProperties: 1}",
		"max properties":   "{type: object, maxProperties: 2}",
		"composition":      "{allOf: [{type: object}]}",
		"array":            "{type: array, items: {type: string}}",
	} {
		t.Run(name, func(t *testing.T) {
			c := responseObjectContext()
			c.Inputs["api.yaml"] = []byte(strings.Replace(string(c.Inputs["api.yaml"]), "metadata: {type: object}", "metadata: "+schema, 1))
			if files, wiring, err := new(Generator).Generate(c); err == nil || files != nil || wiring != nil {
				t.Fatal("unsupported object schema returned output")
			}
		})
	}
}

func TestHTTPResponseObjectGeneration(t *testing.T) {
	c := responseObjectContext()
	g := new(Generator)
	files, wiring, err := g.Generate(c)
	if err != nil {
		t.Fatal(err)
	}
	again, other, err := g.Generate(c)
	if err != nil || !reflect.DeepEqual(files, again) || !reflect.DeepEqual(wiring, other) {
		t.Fatal("object generation is not stable", err)
	}
	counts := map[string]int{}
	for _, f := range files {
		counts[f.Path]++
	}
	for _, name := range []string{"application/responses/json_strings.go", "application/responses/json_objects.go"} {
		if counts[name] != 1 {
			t.Fatal("common JSON decoder missing or repeated", name)
		}
	}
	objectRule(c, 13)["max_bytes"] = (16 << 20) - 512
	objectRule(c, 13)["max_nodes"] = 65536 - 32
	objectRule(c, 13)["max_depth"] = 32
	if err := g.ValidateContext(c); err != nil {
		t.Fatal("exact combined bound rejected", err)
	}
	// Required object fields must reject absent bytes and retain an empty object.
	c = responseObjectContext()
	objectRule(c, 13)["on_empty"] = "reject"
	c.Inputs["api.yaml"] = []byte(strings.Replace(string(c.Inputs["api.yaml"]), "required: [title, count]", "required: [title, count, metadata]", 1))
	if _, _, err := g.Generate(c); err != nil {
		t.Fatal("required object mapping rejected", err)
	}
	for _, c := range []gen.Context{responseContext(), responseJSONContext()} {
		files, _, err := g.Generate(c)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if strings.HasSuffix(f.Path, "/json_objects.go") {
				t.Fatal("unused object decoder generated")
			}
		}
	}
}

func TestGeneratedHTTPResponseObjects(t *testing.T) {
	checkGeneratedResponses(t, responseObjectContext(), responseObjectRuntimeTest,
		[]string{"", "TestObjectPresence", "TestObjectNumbersAndOwnership", "TestObjectRejectsInvalidInput", "TestObjectBounds"})
}

func TestGeneratedHTTPResponseObjectInputs(t *testing.T) {
	c := responseObjectContext()
	m := c.ComponentConfig["response_mappings"].([]any)[0].(map[string]any)
	m["inputs"] = []any{map[string]any{"name": "Payload", "type": "jsonb"}}
	for _, i := range []int{13, 14} {
		r := objectRule(c, i)
		delete(r, "source")
		r["input"] = "Payload"
		r["on_empty"] = "reject"
	}
	checkGeneratedResponses(t, c, responseObjectInputRuntimeTest, []string{"", "TestPreparedObjectInput"})
}

func TestGeneratedHTTPResponseRequiredObjects(t *testing.T) {
	c := responseObjectContext()
	objectRule(c, 13)["on_empty"] = "reject"
	c.Inputs["api.yaml"] = []byte(strings.Replace(string(c.Inputs["api.yaml"]), "required: [title, count]", "required: [title, count, metadata]", 1))
	checkGeneratedResponses(t, c, `package responses
import("encoding/json";"testing";"example.com/dispatch/out/store")
func TestRequiredObject(t *testing.T){
 if r,e:=Shipment(store.Record{});r!=nil||e!=ErrConversion{t.Fatal("required object accepted absent bytes")}
 r,e:=Shipment(store.Record{Tags:[]byte("{}")});if e!=nil||r.Metadata==nil{t.Fatal("empty required object was lost",e)}
 raw,e:=json.Marshal(r.Metadata);if e!=nil||string(raw)!="{}"{t.Fatal("required empty object changed",e)}
}`, []string{"", "TestRequiredObject"})
}

func TestGeneratedHTTPResponseMixedJSON(t *testing.T) {
	c := responseObjectContext()
	m := c.ComponentConfig["response_mappings"].([]any)[0].(map[string]any)
	m["inputs"] = []any{map[string]any{"name": "Labels", "type": "jsonb"}}
	m["fields"].([]any)[11] = map[string]any{"target": "ignored", "input": "Labels", "conversion": "json_strings", "max_bytes": 64, "max_items": 3, "max_item_bytes": 16}
	checkGeneratedResponses(t, c, `package responses
import("testing";"example.com/dispatch/out/store")
func TestMixedJSON(t *testing.T){
 r,e:=Shipment(store.Record{Tags:[]byte("{}")},ShipmentInput{Labels:[]byte("[\"tag\"]")})
 if e!=nil||r.Metadata==nil||r.Ignored==nil||len(*r.Ignored)!=1||(*r.Ignored)[0]!="tag"{t.Fatal("mixed object and list mapping failed",e)}
 r,e=Shipment(store.Record{Tags:[]byte("{}")},ShipmentInput{Labels:[]byte("[1]")})
 if r!=nil||e!=ErrConversion{t.Fatal("invalid list returned a partial object response")}
}`, []string{"", "TestMixedJSON"})
}

const responseObjectRuntimeTest = `package responses
import("bytes";"encoding/json";"strings";"testing";"example.com/dispatch/out/store")
func jsonValue(t *testing.T,v any) map[string]json.RawMessage {t.Helper();raw,e:=json.Marshal(v);if e!=nil{t.Fatal(e)};var result map[string]json.RawMessage;if e=json.Unmarshal(raw,&result);e!=nil{t.Fatal(e)};return result}
func TestObjectPresence(t *testing.T){
 r,e:=Shipment(store.Record{});if e!=nil{t.Fatal(e)};out:=jsonValue(t,r)
 for _,key:=range []string{"metadata","copied_metadata"}{if _,ok:=out[key];ok{t.Fatal("absent object was emitted",key)}}
 for _,raw:=range []string{"{}","{\"a\":null,\"b\":false,\"c\":[],\"d\":{},\"e\":\"\"}","{\"é\":\"\\ud83d\\ude80\"}"}{
  r,e:=Shipment(store.Record{Tags:[]byte(raw)});if e!=nil{t.Fatal(e)};out:=jsonValue(t,r)
  for _,key:=range []string{"metadata","copied_metadata"}{var want,actual any;json.Unmarshal([]byte(raw),&want);json.Unmarshal(out[key],&actual);a,_:=json.Marshal(want);b,_:=json.Marshal(actual);if !bytes.Equal(a,b){t.Fatal("object value or presence changed",key)}}
 }
}
func TestObjectNumbersAndOwnership(t *testing.T){
 raw:=[]byte("{\"a\":[{\"n\":9007199254740993,\"d\":1.234567890123456789,\"z\":-0,\"e\":1e3000}]}");before:=bytes.Clone(raw)
 r,e:=Shipment(store.Record{Tags:raw});if e!=nil{t.Fatal(e)}
 node:=(*r.Metadata)["a"].([]interface{})[0].(map[string]interface{})
 for k,want:=range map[string]string{"n":"9007199254740993","d":"1.234567890123456789","z":"-0","e":"1e3000"}{if n,ok:=node[k].(json.Number);!ok||n.String()!=want{t.Fatal("number changed",k)}}
 encoded,e:=json.Marshal(r.Metadata);if e!=nil||!bytes.Contains(encoded,[]byte("9007199254740993")){t.Fatal("number was rounded during encoding",e)}
 node["n"]=json.Number("1");other:=(*r.CopiedMetadata)["a"].([]interface{})[0].(map[string]interface{})
 if other["n"].(json.Number).String()!="9007199254740993"||!bytes.Equal(raw,before){t.Fatal("object fields or source share mutable storage")}
 for i:=range raw{raw[i]='x'};if other["d"].(json.Number).String()!="1.234567890123456789"{t.Fatal("source mutation changed the object")}
}
func TestObjectRejectsInvalidInput(t *testing.T){
 bad:=[]string{"null","[]","true","1","\"x\""," ","{}{}","{} null","{\"a\":1,\"a\":2}","{\"a\":1,\"\\u0061\":2}","{\"x\":{\"a\":null,\"a\":false}}","{\"a\":NaN}","{\"a\":01}","{\"a\":1e}","{\"a\":1,}","{\"a\":\"\\ud800\"}","{\"\\udc00\":1}","{\"a\":\"\\ud800\\u0041\"}","{\"a\":\""+string([]byte{255})+"\"}","{}"+strings.Repeat(" ",511)}
 for i,raw:=range bad{r,e:=Shipment(store.Record{Tags:[]byte(raw)});if r!=nil||e!=ErrConversion||e.Error()!="response conversion failed"{t.Fatal("invalid object returned partial data or a variable error",i)}}
}
func TestObjectBounds(t *testing.T){
 cases:=[]struct{raw string;b,n,d,s int;valid bool}{
  {"{}",2,1,1,1,true},{"{} ",2,1,1,1,false},
  {"{\"a\":null}",20,3,1,1,true},{"{\"a\":null}",20,2,1,1,false},
  {"{\"a\":[1,2]}",20,5,2,1,true},{"{\"a\":[1,2]}",20,4,2,1,false},
  {"{\"a\":{}}",20,3,2,1,true},{"{\"a\":{}}",20,3,1,1,false},
  {"{\"a\":[{}]}",20,4,3,1,true},{"{\"a\":[{}]}",20,4,2,1,false},
  {"{\"é\":\"é\"}",20,3,1,2,true},{"{\"é\":1}",20,3,1,1,false},
  {"{\"a\":\"é\"}",20,3,1,1,false},{"{\"a\":12}",20,3,1,2,true},{"{\"a\":12}",20,3,1,1,false},
  {"{\"a\":\"\\u0061\"}",20,3,1,1,true},
 }
 for i,c:=range cases{v,e:=jsonObject([]byte(c.raw),c.b,c.n,c.d,c.s);if c.valid{if e!=nil||v==nil{t.Fatal("valid exact bound rejected",i)}}else if v!=nil||e!=ErrConversion{t.Fatal("bound violation returned data",i)}}
 for depth:=32;depth<=33;depth++{raw:=strings.Repeat("{\"a\":",depth-1)+"{}"+strings.Repeat("}",depth-1);v,e:=jsonObject([]byte(raw),1024,128,32,1);if depth==32{if e!=nil||v==nil{t.Fatal("maximum depth rejected")}}else if e!=ErrConversion||v!=nil{t.Fatal("excess depth returned data")}}
}
`

const responseObjectInputRuntimeTest = `package responses
import("encoding/json";"testing";"example.com/dispatch/out/store")
func TestPreparedObjectInput(t *testing.T){
 for _,raw:=range []string{"","null","[]","{\"x\":1,\"x\":2}"}{r,e:=Shipment(store.Record{},ShipmentInput{Payload:[]byte(raw)});if r!=nil||e!=ErrConversion{t.Fatal("invalid prepared object returned data")}}
 raw:=[]byte("{}");r,e:=Shipment(store.Record{},ShipmentInput{Payload:raw});if e!=nil||r.Metadata==nil||r.CopiedMetadata==nil{t.Fatal("present empty input disappeared",e)}
 data,e:=json.Marshal(r.Metadata);if e!=nil||string(data)!="{}"{t.Fatal("empty input became null",e)}
 (*r.Metadata)["new"]=true;if len(*r.CopiedMetadata)!=0{t.Fatal("prepared response objects share storage")}
}
`
