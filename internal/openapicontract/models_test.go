package openapicontract

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
)

func TestGoResponseObjectBindings(t *testing.T) {
	doc, err := goDocument(goFixture)
	if err != nil {
		t.Fatal(err)
	}
	source, objects, err := GenerateGoModels(doc, "responses", []string{"Shipment", "Reference"})
	if err != nil {
		t.Fatal(err)
	}
	want := GoObject{GoType: "Shipment", Fields: []GoProperty{
		{JSONName: "description", GoName: "Description", GoType: "*string", OmitEmpty: true},
		{JSONName: "enabled", GoName: "Enabled", GoType: "nullable.Nullable[bool]", Nullable: true, OmitEmpty: true},
		{JSONName: "serial", GoName: "Serial", GoType: "string", Required: true},
		{JSONName: "tags", GoName: "Tags", GoType: "[]string", Required: true, StringList: true},
		{JSONName: "updated_at", GoName: "UpdatedAt", GoType: "*time.Time", OmitEmpty: true},
	}}
	if !reflect.DeepEqual(objects["Shipment"], want) || len(objects) != 2 {
		t.Fatalf("unexpected object bindings: %#v", objects)
	}
	plain, err := GenerateGo(doc, "responses", GoModels)
	if err != nil || plain != source {
		t.Fatal("bindings changed generated model code", err)
	}
	objects["Shipment"].Fields[0].JSONName = "changed"
	again, other, err := GenerateGoModels(doc, "responses", []string{"Reference", "Shipment"})
	if err != nil || again != source || !reflect.DeepEqual(other["Shipment"], want) {
		t.Fatal("binding mutation or selection order changed another result", err)
	}
}

func TestGoResponseBindingsRejectAmbiguity(t *testing.T) {
	for name, source := range map[string]string{
		"composed property": strings.Replace(goFixture, "required: [tags]", "required: [tags]\n          properties:\n            serial: {type: string}\n        - type: object", 1),
		"field collision":   strings.Replace(goFixture, "description: {type: string}", "first_name: {type: string}\n            firstName: {type: string}", 1),
		"type collision":    goFixture + "    shipment:\n      type: object\n      properties:\n        note: {type: string}\n",
		"open object":       strings.Replace(goFixture, "required: [tags]", "additionalProperties: true\n          required: [tags]", 1),
		"nullable object":   strings.Replace(goFixture, "required: [tags]", "nullable: true\n          required: [tags]", 1),
		"union object":      strings.Replace(goFixture, "      allOf:", "      oneOf:", 1),
	} {
		t.Run(name, func(t *testing.T) {
			doc, err := goDocument(source)
			if err != nil {
				t.Fatal("the negative binding fixture did not load:", err)
			}
			code, objects, err := GenerateGoModels(doc, "responses", []string{"Shipment"})
			if err == nil || code != "" || objects != nil {
				t.Fatal("invalid response bindings returned output")
			}
		})
	}
	doc, err := goDocument(goFixture)
	if err != nil {
		t.Fatal(err)
	}
	for _, selection := range [][]string{nil, {}, {"Unknown"}, {"Shipment", "Shipment"}, make([]string, 129)} {
		if code, objects, err := GenerateGoModels(doc, "responses", selection); err == nil || code != "" || objects != nil {
			t.Fatal("invalid selection returned response bindings")
		}
	}
	if code, objects, err := GenerateGoModels(nil, "responses", []string{"Shipment"}); err == nil || code != "" || objects != nil {
		t.Fatal("missing document returned response bindings")
	}
}

func TestGoResponseBindingsCapturedReferences(t *testing.T) {
	root := `openapi: 3.0.3
info: {title: Courier, version: '1'}
paths:
  /parcels:
    get:
      operationId: getParcel
      responses:
        '200':
          description: Parcel
          content:
            application/json:
              schema: {$ref: '#/components/schemas/Parcel'}
components:
  schemas:
    Parcel: {$ref: 'common.yaml#/components/schemas/Record'}
`
	common := `openapi: 3.0.3
info: {title: Common, version: '1'}
paths: {}
components:
  schemas:
    Record:
      type: object
      required: [tracking_code]
      properties:
        tracking_code: {type: string}
`
	doc, err := Load(gen.Context{ComponentConfig: map[string]any{"document": "api.yaml", "references": []any{"common.yaml"}}, Inputs: map[string][]byte{"api.yaml": []byte(root), "common.yaml": []byte(common)}})
	if err != nil {
		t.Fatal(err)
	}
	_, bindings, err := GenerateGoModels(doc, "responses", []string{"Parcel"})
	if err != nil {
		t.Fatal(err)
	}
	want := GoObject{GoType: "Parcel", Fields: []GoProperty{{JSONName: "tracking_code", GoName: "TrackingCode", GoType: "string", Required: true}}}
	if !reflect.DeepEqual(bindings["Parcel"], want) {
		t.Fatalf("captured reference has wrong bindings: %#v", bindings)
	}
}

func TestGoResponseBindingsConcurrentBackend(t *testing.T) {
	start := make(chan struct{})
	results := make(chan error, 4)
	for _, kind := range []string{"Shipment", "Parcel"} {
		for _, bind := range []bool{false, true} {
			go func() {
				<-start
				doc, err := goDocument(strings.ReplaceAll(goFixture, "Shipment", kind))
				if err == nil && bind {
					var objects map[string]GoObject
					_, objects, err = GenerateGoModels(doc, "responses", []string{kind})
					if err == nil && (objects[kind].GoType != kind || len(objects[kind].Fields) != 5) {
						err = fmt.Errorf("concurrent response binding changed")
					}
				} else if err == nil {
					_, err = GenerateGo(doc, "wire", GoClient)
				}
				results <- err
			}()
		}
	}
	close(start)
	for range 4 {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
}
