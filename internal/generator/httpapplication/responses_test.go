package httpapplication

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/types"
)

const responseFixture = `openapi: 3.0.3
info: {title: Dispatch, version: '1'}
paths:
  /shipments:
    get:
      operationId: getShipment
      responses:
        '200':
          description: Shipment
          content:
            application/json:
              schema: {$ref: '#/components/schemas/Shipment'}
components:
  schemas:
    Reference:
      type: object
      properties:
        id: {type: string}
        kind: {type: string}
        href: {type: string}
        created: {type: string, format: date-time}
    Shipment:
      allOf:
        - {$ref: '#/components/schemas/Reference'}
        - type: object
          required: [title, count]
          properties:
            title: {type: string}
            count: {type: integer, format: int32}
            note: {type: string}
            enabled: {type: boolean}
            active: {type: boolean}
            creator: {type: string}
            score: {type: number, format: double}
            ignored: {type: array, items: {type: string}}
            nullable_note: {type: string, nullable: true}
`

func responseContext() gen.Context {
	fields := []any{
		map[string]any{"target": "id", "source": "id", "omit_empty": true},
		map[string]any{"target": "kind", "constant": "Shipment"},
		map[string]any{"target": "href", "source": "id", "prefix": "/shipments/"},
		map[string]any{"target": "created", "source": "created"},
		map[string]any{"target": "title", "source": "title"},
		map[string]any{"target": "count", "source": "count", "conversion": "int32"},
		map[string]any{"target": "note", "source": "note"},
		map[string]any{"target": "enabled", "source": "enabled"},
		map[string]any{"target": "active", "source": "active"},
		map[string]any{"target": "creator", "source": "creator", "omit_empty": true},
		map[string]any{"target": "score", "source": "score"},
		map[string]any{"target": "ignored", "omit": true},
		map[string]any{"target": "nullable_note", "omit": true},
	}
	return gen.Context{
		ModuleName: "example.com/dispatch", OutDirName: "out", OutputNamespace: "application",
		StorageContract: "example.com/dispatch/out/contracts/storage", AuthPackage: "example.com/dispatch/out/auth",
		PeerNamespaces:  map[string]string{"jwt-auth": "auth"},
		ComponentConfig: map[string]any{"factory_package": "domain", "document": "api.yaml", "response_mappings": []any{map[string]any{"name": "Shipment", "provider": "custom-store", "model": "record", "schema": "Shipment", "fields": fields}}},
		Inputs:          map[string][]byte{"api.yaml": []byte(responseFixture)},
		GoModelSources: map[string]gen.GoModelSource{"custom-store": {ImportPath: "example.com/dispatch/out/store", Models: []gen.GoModel{{Name: "record", GoType: "Record", Fields: []gen.GoModelField{
			{Name: "id", Selection: "ID", Type: types.FieldTypeString},
			{Name: "created", Selection: "CreatedTime", Type: types.FieldTypeTimestamp},
			{Name: "title", Selection: "Title", Type: types.FieldTypeString},
			{Name: "count", Selection: "Count", Type: types.FieldTypeInt64},
			{Name: "note", Selection: "Note", Type: types.FieldTypeString, Pointer: true},
			{Name: "enabled", Selection: "Enabled", Type: types.FieldTypeBool},
			{Name: "active", Selection: "Active", Type: types.FieldTypeBool, Pointer: true},
			{Name: "creator", Selection: "Creator", Type: types.FieldTypeString},
			{Name: "score", Selection: "Score", Type: types.FieldTypeDouble},
		}}}}},
	}
}

func TestHTTPResponseMappingValidation(t *testing.T) {
	mapping := func(c gen.Context) map[string]any {
		return c.ComponentConfig["response_mappings"].([]any)[0].(map[string]any)
	}
	field := func(c gen.Context, i int) map[string]any { return mapping(c)["fields"].([]any)[i].(map[string]any) }
	for name, change := range map[string]func(gen.Context){
		"missing document":           func(c gen.Context) { delete(c.ComponentConfig, "document") },
		"missing mappings":           func(c gen.Context) { delete(c.ComponentConfig, "response_mappings") },
		"missing input":              func(c gen.Context) { delete(c.Inputs, "api.yaml") },
		"missing provider":           func(c gen.Context) { delete(c.GoModelSources, "custom-store") },
		"unknown model":              func(c gen.Context) { mapping(c)["model"] = "absent" },
		"unknown schema":             func(c gen.Context) { mapping(c)["schema"] = "Absent" },
		"unknown target":             func(c gen.Context) { field(c, 0)["target"] = "absent" },
		"duplicate target":           func(c gen.Context) { field(c, 1)["target"] = "id" },
		"missing target":             func(c gen.Context) { mapping(c)["fields"] = mapping(c)["fields"].([]any)[1:] },
		"unknown option":             func(c gen.Context) { field(c, 0)["unsafe"] = true },
		"unknown source":             func(c gen.Context) { field(c, 0)["source"] = "absent" },
		"private name":               func(c gen.Context) { mapping(c)["name"] = "shipment" },
		"reserved name":              func(c gen.Context) { mapping(c)["name"] = "ErrConversion" },
		"duplicate mapping":          func(c gen.Context) { c.ComponentConfig["response_mappings"] = []any{mapping(c), mapping(c)} },
		"private model":              func(c gen.Context) { c.GoModelSources["custom-store"].Models[0].GoType = "record" },
		"expression source":          func(c gen.Context) { c.GoModelSources["custom-store"].Models[0].Fields[0].Selection = "ID()" },
		"narrowing without rule":     func(c gen.Context) { delete(field(c, 5), "conversion") },
		"unknown conversion":         func(c gen.Context) { field(c, 5)["conversion"] = "int16" },
		"required omission":          func(c gen.Context) { mapping(c)["fields"].([]any)[4] = map[string]any{"target": "title", "omit": true} },
		"false omission":             func(c gen.Context) { field(c, 11)["omit"] = false },
		"optional to required":       func(c gen.Context) { c.GoModelSources["custom-store"].Models[0].Fields[2].Pointer = true },
		"prefix on pointer":          func(c gen.Context) { field(c, 6)["prefix"] = "/notes/" },
		"omit present empty pointer": func(c gen.Context) { field(c, 6)["omit_empty"] = true },
		"constant with source":       func(c gen.Context) { field(c, 1)["source"] = "title" },
		"invalid constant encoding":  func(c gen.Context) { field(c, 1)["constant"] = string([]byte{255}) },
		"nullable source mismatch": func(c gen.Context) {
			mapping(c)["fields"].([]any)[12] = map[string]any{"target": "nullable_note", "source": "note"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := responseContext()
			change(ctx)
			g := new(Generator)
			if err := g.ValidateContext(ctx); err == nil {
				t.Fatal("invalid mapping passed context validation")
			}
			files, wiring, err := g.Generate(ctx)
			if err == nil || files != nil || wiring != nil {
				t.Fatal("invalid mapping returned generated output")
			}
		})
	}
}

func TestHTTPResponseInputsAndDeterminism(t *testing.T) {
	ctx := responseContext()
	g := new(Generator)
	inputs, err := g.InputFiles(ctx.ComponentConfig)
	if err != nil || !reflect.DeepEqual(inputs, gen.SourceInputs([]string{"api.yaml"})) {
		t.Fatal("response contract inputs were not captured", err)
	}
	files, wiring, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	again, other, err := g.Generate(ctx)
	if err != nil || !reflect.DeepEqual(files, again) || !reflect.DeepEqual(wiring, other) {
		t.Fatal("response generation changed on repeat", err)
	}
	without := responseContext()
	delete(without.ComponentConfig, "document")
	delete(without.ComponentConfig, "response_mappings")
	old, _, err := g.Generate(without)
	if err != nil || len(files) != len(old)+2 {
		t.Fatal("unexpected response output files", err)
	}
	for i := range old {
		if !reflect.DeepEqual(files[i], old[i]) {
			t.Fatal("response mapping changed an existing HTTP runtime file")
		}
	}
	if wiring.GoModRequires["github.com/oapi-codegen/nullable"] != "v1.1.0" || wiring.GoModRequires["github.com/oapi-codegen/runtime"] != "v1.7.0" {
		t.Fatal("response model dependencies are not pinned")
	}
}

func TestGeneratedHTTPResponseMappings(t *testing.T) {
	ctx := responseContext()
	files, wiring, err := new(Generator).Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	write := func(name string, data []byte) {
		t.Helper()
		file := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(file), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range files {
		if strings.HasPrefix(file.Path, "application/contract/") || strings.HasPrefix(file.Path, "application/responses/") {
			write("out/"+file.Path, file.Bytes())
		}
	}
	write("out/store/model.go", []byte(`package store
import "time"
type Record struct {ID,Title,Creator string; CreatedTime time.Time; Count int64; Note *string; Enabled bool; Active *bool; Score float64}
`))
	write("out/application/responses/response_test.go", []byte(responseRuntimeTest))
	var mod strings.Builder
	mod.WriteString("module example.com/dispatch\ngo 1.26.0\nrequire(\n")
	var modules []string
	for name := range wiring.GoModRequires {
		modules = append(modules, name)
	}
	sort.Strings(modules)
	for _, name := range modules {
		fmt.Fprintf(&mod, "%s %s\n", name, wiring.GoModRequires[name])
	}
	mod.WriteString(")\n")
	write("go.mod", []byte(mod.String()))
	for _, args := range [][]string{{"mod", "tidy"}, {"test", "-json", "-race", "-count=1", "-mod=readonly", "-timeout=60s", "./..."}} {
		cmd := exec.Command("go", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GOWORK=off")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		output, err := cmd.Output()
		if err != nil {
			t.Fatalf("generated response checks: %v\n%s\n%s", err, output, stderr.String())
		}
		if args[0] != "test" {
			continue
		}
		passed := map[string]bool{}
		decoder := json.NewDecoder(bytes.NewReader(output))
		for {
			var event struct{ Action, Package, Test string }
			if err := decoder.Decode(&event); err == io.EOF {
				break
			} else if err != nil {
				t.Fatal(err)
			}
			if event.Action == "fail" || (event.Action == "skip" && event.Test != "") {
				t.Fatal("generated response test did not pass", event)
			}
			if event.Action == "pass" && event.Package == "example.com/dispatch/out/application/responses" {
				passed[event.Test] = true
			}
		}
		for _, name := range []string{"", "TestResponsePresenceAndOwnership", "TestResponseInvalidValues", "TestResponseIntegerBounds"} {
			if !passed[name] {
				t.Fatal("missing generated response result", name)
			}
		}
		t.Log("generated response presence, ownership, invalid values, and integer bounds passed")
	}
}

const responseRuntimeTest = `package responses
import("encoding/json";"math";"reflect";"testing";"time";"example.com/dispatch/out/store")
func row() store.Record {return store.Record{ID:"ship-1", Title:"", CreatedTime:time.Date(2026,9,21,1,2,3,123456789,time.UTC)}}
func jsonValue(t *testing.T,v any) map[string]json.RawMessage {t.Helper();raw,e:=json.Marshal(v);if e!=nil{t.Fatal(e)};var result map[string]json.RawMessage;if e=json.Unmarshal(raw,&result);e!=nil{t.Fatal(e)};return result}
func TestResponsePresenceAndOwnership(t *testing.T){
 input:=row();r,e:=Shipment(input);if e!=nil{t.Fatal(e)}
 want:=map[string]json.RawMessage{"id":[]byte("\"ship-1\""),"kind":[]byte("\"Shipment\""),"href":[]byte("\"/shipments/ship-1\""),"created":[]byte("\"2026-09-21T01:02:03.123456789Z\""),"title":[]byte("\"\""),"count":[]byte("0"),"enabled":[]byte("false"),"score":[]byte("0")}
 if got:=jsonValue(t,r);!reflect.DeepEqual(got,want){t.Fatalf("wrong response properties: %s",got)}
 note:="";active:=false;input.Note=&note;input.Active=&active;input.Creator="person";r,e=Shipment(input);if e!=nil{t.Fatal(e)}
 got:=jsonValue(t,r);if string(got["note"])!="\"\""||string(got["active"])!="false"||string(got["creator"])!="\"person\""{t.Fatal("present empty values were lost")}
 *r.Note="changed";*r.Active=true;if note!=""||active{t.Fatal("response shares source pointers")}
 note="later";active=true;if *r.Note!="changed"{t.Fatal("source changes changed the response")}
 input=row();input.ID="";r,e=Shipment(input);if e!=nil{t.Fatal(e)};if _,ok:=jsonValue(t,r)["id"];ok{t.Fatal("explicit empty omission was ignored")}
 input.CreatedTime=time.Date(2026,9,21,1,2,3,9,time.FixedZone("offset",3600));r,e=Shipment(input);if e!=nil{t.Fatal(e)};if string(jsonValue(t,r)["created"])!="\"2026-09-21T01:02:03.000000009+01:00\""{t.Fatal("timestamp offset or precision changed")}
}
func TestResponseInvalidValues(t *testing.T){
 for name,change:=range map[string]func(*store.Record){
 "title":func(v *store.Record){v.Title=string([]byte{255})},"pointer":func(v *store.Record){s:=string([]byte{255});v.Note=&s},"prefix":func(v *store.Record){v.ID=string([]byte{255})},"year":func(v *store.Record){v.CreatedTime=time.Date(10000,1,1,0,0,0,0,time.UTC)},"zone seconds":func(v *store.Record){v.CreatedTime=v.CreatedTime.In(time.FixedZone("seconds",30))},"nan":func(v *store.Record){v.Score=math.NaN()},"infinity":func(v *store.Record){v.Score=math.Inf(1)},
 }{t.Run(name,func(t *testing.T){v:=row();change(&v);r,e:=Shipment(v);if r!=nil||e!=ErrConversion||e.Error()!="response conversion failed"{t.Fatal("invalid value returned data or a variable error")}})}
}
func TestResponseIntegerBounds(t *testing.T){for _,n:=range []int64{-2147483649,-2147483648,0,2147483647,2147483648}{v:=row();v.Count=n;r,e:=Shipment(v);if n < -2147483648||n>2147483647{if e!=ErrConversion||r!=nil{t.Fatal("overflow returned output")}}else if e!=nil||int64(r.Count)!=n{t.Fatal("valid integer changed")}}}
`
