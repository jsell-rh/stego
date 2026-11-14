package restapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/jsell-rh/stego/internal/generator/spicedbauthz"
)

// revocationOperation is what the test reads of one operation of the OpenAPI document.
type revocationOperation struct {
	Parameters []struct {
		Name     string `json:"name"`
		In       string `json:"in"`
		Required bool   `json:"required"`
	} `json:"parameters"`
	Responses map[string]struct {
		Headers map[string]struct {
			Schema struct {
				Enum []string `json:"enum"`
			} `json:"schema"`
		} `json:"headers"`
		Content map[string]struct {
			Schema struct {
				Required   []string                   `json:"required"`
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"schema"`
		} `json:"content"`
	} `json:"responses"`
}

// revocationDocument generates the rest-api component of the todo test declaration and gives its OpenAPI document
// as bytes and as the parts that ST-SUP-RC20-OC43 states.
func revocationDocument(t *testing.T) ([]byte, map[string]map[string]revocationOperation) {
	t.Helper()
	files, _, err := new(Generator).Generate(onRuntime(todoIdempotencyContext()))
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, file := range files {
		if file.Path != "internal/api/openapi.json" {
			continue
		}
		var document struct {
			Paths map[string]map[string]revocationOperation `json:"paths"`
		}
		if err := json.Unmarshal(file.Bytes(), &document); err != nil {
			t.Fatalf("read openapi.json: %v", err)
		}
		return file.Bytes(), document.Paths
	}
	t.Fatal("the rest-api component emits no openapi.json")
	return nil, nil
}

func TestRevocationDocument(t *testing.T) {
	t.Run("ST-SUP-RC20-OC43-T01 The OpenAPI document describes the revocation headers of each operation and the status route", func(t *testing.T) {
		data, paths := revocationDocument(t)
		const collection, item = "/api/todo/v1/todos", "/api/todo/v1/todos/{id}"
		for _, read := range []struct{ path, name string }{{collection, "list"}, {item, "read"}} {
			op := paths[read.path]["get"]
			headers := map[string]bool{}
			for _, p := range op.Parameters {
				if p.In == "header" && !p.Required {
					headers[p.Name] = true
				}
			}
			if !headers["Authz-Revocation-After"] || !headers["Prefer"] {
				t.Errorf("the %s takes the optional headers %v, want Authz-Revocation-After and Prefer", read.name, headers)
			}
			if _, ok := op.Responses["400"]; !ok {
				t.Errorf("the %s has no 400 response for a bad Authz-Revocation-After", read.name)
			}
			state := op.Responses["200"].Headers["Authz-Revocation"]
			if strings.Join(state.Schema.Enum, ",") != "pending,complete,parked,failed,expired,unknown" {
				t.Errorf("the 200 of the %s answers Authz-Revocation with %v, want the states and unknown", read.name, state.Schema.Enum)
			}
		}
		for _, write := range []struct{ path, method, code string }{
			{collection, "post", "201"}, {item, "put", "200"}, {item, "patch", "200"}, {item, "delete", "204"},
		} {
			response := paths[write.path][write.method].Responses[write.code]
			if _, ok := response.Headers["Authz-Revocation-Location"]; !ok || strings.Join(response.Headers["Authz-Revocation"].Schema.Enum, ",") != "pending" {
				t.Errorf("the %s %s answers %s with the headers %v, want Authz-Revocation: pending and Authz-Revocation-Location", write.method, write.path, write.code, response.Headers)
			}
		}
		// The status route is the route that the spicedb-authz wiring registers.
		route := strings.TrimPrefix(spicedbauthz.RevocationRoute, "GET ")
		if route == spicedbauthz.RevocationRoute || revocationRoute != route {
			t.Errorf("the document names the status route %s, but spicedb-authz registers %s", revocationRoute, spicedbauthz.RevocationRoute)
		}
		status, ok := paths[route]["get"]
		if !ok {
			t.Fatalf("the document has no status route; its paths are %v", paths)
		}
		params := map[string]string{}
		for _, p := range status.Parameters {
			params[p.Name] = p.In
		}
		if params["id"] != "path" || params["Prefer"] != "header" {
			t.Errorf("the status route takes %v, want the path parameter id and the header Prefer", params)
		}
		ok200 := status.Responses["200"]
		body := ok200.Content["application/json"].Schema
		if _, ok := ok200.Headers["Authz-Revocation"]; !ok || strings.Join(body.Required, ",") != "id,state" || len(body.Properties) != 2 {
			t.Errorf("the 200 of the status route has the headers %v and the body %v, want Authz-Revocation and a body of id and state only", ok200.Headers, body)
		}
		// The status route answers each state that authz.Status gives, expired among them, and never unknown.
		states := "pending,complete,parked,failed,expired"
		if got := strings.Join(ok200.Headers["Authz-Revocation"].Schema.Enum, ","); got != states {
			t.Errorf("the 200 of the status route answers Authz-Revocation with %s, want %s", got, states)
		}
		var state struct {
			Enum []string `json:"enum"`
		}
		_ = json.Unmarshal(body.Properties["state"], &state)
		if got := strings.Join(state.Enum, ","); got != states {
			t.Errorf("the body of the status route has the states %s, want %s", got, states)
		}
		if _, ok := status.Responses["404"]; !ok {
			t.Error("the status route has no 404")
		}
		// The document is a valid OpenAPI 3.0 document.
		doc, err := openapi3.NewLoader().LoadFromData(data)
		if err != nil {
			t.Fatalf("load the document: %v", err)
		}
		if err := doc.Validate(context.Background()); err != nil {
			t.Errorf("the document is not valid: %v", err)
		}
	})
}
