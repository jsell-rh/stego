package openapicontract

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/jsell-rh/stego/internal/gen"
	"github.com/oapi-codegen/oapi-codegen/v2/pkg/codegen"
)

const goFixture = `openapi: 3.0.3
info: {title: Shipping, version: '1'}
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
      required: [serial]
      properties:
        serial: {type: string}
    Shipment:
      allOf:
        - {$ref: '#/components/schemas/Reference'}
        - type: object
          required: [tags]
          properties:
            description: {type: string}
            enabled: {type: boolean, nullable: true}
            updated_at: {type: string, format: date-time}
            tags: {type: array, items: {type: string}}
    Unused:
      type: object
      properties:
        count: {type: integer, format: int64}
`

func goDocument(source string) (*openapi3.T, error) {
	return Load(gen.Context{
		ComponentConfig: map[string]any{"document": "api.yaml"},
		Inputs:          map[string][]byte{"api.yaml": []byte(source)},
	})
}

func TestGoClientPreservesPreviousBackendOutput(t *testing.T) {
	for _, source := range []string{goFixture, strings.ReplaceAll(goFixture, "Shipment", "Parcel")} {
		doc, err := goDocument(source)
		if err != nil {
			t.Fatal(err)
		}
		got, err := GenerateGo(doc, "wire", GoClient)
		if err != nil {
			t.Fatal(err)
		}
		// Retain the SDK configuration from before the common backend extraction.
		// Compare the complete bytes, including imports and operation wrappers.
		prior, err := goDocument(source)
		if err != nil {
			t.Fatal(err)
		}
		version := "stego/openapi-backend-v2.8.0"
		goBackendMu.Lock()
		want, err := codegen.Generate(prior, codegen.Configuration{PackageName: "wire", Generate: codegen.GenerateOptions{Models: true, Client: true}, OutputOptions: codegen.OutputOptions{SkipPrune: true, NullableType: true}, NoVCSVersionOverride: &version})
		goBackendMu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatal("the common backend changed SDK output")
		}
	}
}

func TestGoModelsPreservePresenceWithoutClient(t *testing.T) {
	doc, err := goDocument(goFixture)
	if err != nil {
		t.Fatal(err)
	}
	source, err := GenerateGo(doc, "responses", GoModels)
	if err != nil {
		t.Fatal(err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), "responses.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	if file.Name.Name != "responses" {
		t.Fatal("the requested package was not used")
	}
	for _, item := range file.Imports {
		name, err := strconv.Unquote(item.Path.Value)
		if err != nil || name == "net/http" || name == "context" {
			t.Fatal("model output contains a client import")
		}
	}
	fields := map[string]string{}
	unused := false
	for _, declaration := range file.Decls {
		if fn, ok := declaration.(*ast.FuncDecl); ok && fn.Name.Name == "NewClient" {
			t.Fatal("model output contains an HTTP client")
		}
		declaration, ok := declaration.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range declaration.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			if typeSpec.Name.Name == "Unused" {
				unused = true
			}
			if typeSpec.Name.Name != "Shipment" {
				continue
			}
			structure, ok := typeSpec.Type.(*ast.StructType)
			if !ok {
				t.Fatal("the response model is not a structure")
			}
			for _, field := range structure.Fields.List {
				if field.Tag == nil || len(field.Names) != 1 {
					t.Fatal("unexpected response field")
				}
				tag, err := strconv.Unquote(field.Tag.Value)
				if err != nil {
					t.Fatal(err)
				}
				name := reflect.StructTag(tag).Get("json")
				if _, exists := fields[name]; exists {
					t.Fatal("duplicate response property")
				}
				var body bytes.Buffer
				if err := format.Node(&body, token.NewFileSet(), field.Type); err != nil {
					t.Fatal(err)
				}
				fields[name] = body.String()
			}
		}
	}
	want := map[string]string{
		"serial":                "string",
		"description,omitempty": "*string",
		"enabled,omitempty":     "nullable.Nullable[bool]",
		"updated_at,omitempty":  "*time.Time",
		"tags":                  "[]string",
	}
	if !reflect.DeepEqual(fields, want) || !unused {
		t.Fatalf("model field or pruning policy changed: %v; unused model: %v", fields, unused)
	}
}

func TestGoBackendRejectsInvalidSelection(t *testing.T) {
	doc, err := goDocument(goFixture)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", "_", "for", "a/b", "a.b", "a\npackage evil", strings.Repeat("a", 129)} {
		if source, err := GenerateGo(doc, name, GoModels); err == nil || source != "" {
			t.Fatal("invalid package selection produced output")
		}
	}
	for _, mode := range []GoMode{0, 3, 255} {
		if source, err := GenerateGo(doc, "responses", mode); err == nil || source != "" {
			t.Fatal("invalid mode produced output")
		}
	}
	for _, doc := range []*openapi3.T{nil, {}} {
		if source, err := GenerateGo(doc, "responses", GoModels); err == nil || source != "" {
			t.Fatal("missing loaded document produced output")
		}
	}
}

func TestGoBackendConcurrentProfiles(t *testing.T) {
	type request struct {
		source, pkg, expected string
		mode                  GoMode
	}
	var requests []request
	for _, kind := range []string{"Shipment", "Parcel"} {
		for _, mode := range []GoMode{GoModels, GoClient} {
			r := request{source: strings.ReplaceAll(goFixture, "Shipment", kind), pkg: strings.ToLower(kind), mode: mode}
			doc, err := goDocument(r.source)
			if err != nil {
				t.Fatal(err)
			}
			r.expected, err = GenerateGo(doc, r.pkg, r.mode)
			if err != nil {
				t.Fatal(err)
			}
			requests = append(requests, r)
		}
	}
	start := make(chan struct{})
	results := make(chan error, len(requests))
	for _, r := range requests {
		go func() {
			<-start
			doc, err := goDocument(r.source)
			if err != nil {
				results <- err
				return
			}
			source, err := GenerateGo(doc, r.pkg, r.mode)
			if err == nil && source != r.expected {
				err = fmt.Errorf("concurrent model and client generation changed output")
			}
			results <- err
		}()
	}
	close(start)
	for range requests {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
}
