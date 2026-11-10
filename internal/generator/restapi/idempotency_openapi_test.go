package restapi

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/types"
)

const todoProblemBase = "https://errors.example.com/todo/"

// todoIdempotencyContext is the test declaration of the idempotency component's output contracts: the owned entity
// Todo in the collection todos with create, read, list, patch, update and delete, the problem base, and the
// idempotency component in the service with Required on as given.
func todoIdempotencyContext(requiredOn ...types.Operation) gen.Context {
	return gen.Context{
		Conventions:     types.Convention{ErrorHandling: "problem-details-rfc"},
		OutputNamespace: "internal/api",
		BasePath:        "/api/todo/v1",
		ErrorTypeBase:   todoProblemBase,
		Idempotency:     &types.Idempotency{RequiredOn: requiredOn, Retention: "24h", Fingerprint: "body"},
		PeerNamespaces:  map[string]string{"idempotency": "internal/idempotency"},
		Entities: []types.Entity{{Name: "Todo", Fields: []types.Field{
			{Name: "title", Type: types.FieldTypeString},
			{Name: "done", Type: types.FieldTypeBool, Optional: true},
		}}},
		Collections: []types.Collection{{Name: "todos", Entity: "Todo", Patchable: []string{"title", "done"},
			Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpList, types.OpPatch, types.OpUpdate, types.OpDelete}}},
	}
}

type documentOperation struct {
	Parameters []struct {
		Name     string `json:"name"`
		In       string `json:"in"`
		Required bool   `json:"required"`
	} `json:"parameters"`
	Responses map[string]struct {
		Description string `json:"description"`
	} `json:"responses"`
}

// todoOperations generates the rest-api component and reads the operations of its OpenAPI document by method and
// path.
func todoOperations(t *testing.T, ctx gen.Context) map[string]documentOperation {
	t.Helper()
	files, _, err := new(Generator).Generate(ctx)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, file := range files {
		if file.Path != "internal/api/openapi.json" {
			continue
		}
		var document struct {
			Paths map[string]map[string]documentOperation `json:"paths"`
		}
		if err := json.Unmarshal(file.Bytes(), &document); err != nil {
			t.Fatalf("read openapi.json: %v", err)
		}
		operations := map[string]documentOperation{}
		for path, item := range document.Paths {
			for method, operation := range item {
				operations[strings.ToUpper(method)+" "+path] = operation
			}
		}
		return operations
	}
	t.Fatal("the rest-api component emits no openapi.json")
	return nil
}

// keyHeader gives the Idempotency-Key header parameter of an operation, and whether it has one.
func keyHeader(operation documentOperation) (required, found bool) {
	for _, parameter := range operation.Parameters {
		if parameter.Name == "Idempotency-Key" {
			if parameter.In != "header" {
				return false, false
			}
			return parameter.Required, true
		}
	}
	return false, false
}

// checkProblems expects the 400, 409 and 422 responses of an operation to name the idempotency problem types.
func checkProblems(t *testing.T, label string, operation documentOperation) {
	t.Helper()
	for code, slug := range map[string]string{"400": "idempotency-key-missing", "409": "idempotency-key-in-progress", "422": "idempotency-key-reused"} {
		if !strings.Contains(operation.Responses[code].Description, todoProblemBase+slug) {
			t.Errorf("%s: response %s is %q, want the problem type %s", label, code, operation.Responses[code].Description, todoProblemBase+slug)
		}
	}
}

func TestIdempotencyKeyHeader(t *testing.T) {
	t.Run("ST-SUP-RC30-OC01-T01 Create requires the Idempotency-Key header in the OpenAPI document", func(t *testing.T) {
		create := todoOperations(t, todoIdempotencyContext(types.OpCreate, types.OpPatch))["POST /api/todo/v1/todos"]
		if required, found := keyHeader(create); !found || !required {
			t.Fatalf("create: Idempotency-Key header found %t, required %t; want a required header", found, required)
		}
		checkProblems(t, "create", create)
	})

	t.Run("ST-SUP-RC30-OC01-T02 Patch accepts the Idempotency-Key header", func(t *testing.T) {
		patch := todoOperations(t, todoIdempotencyContext(types.OpCreate))["PATCH /api/todo/v1/todos/{id}"]
		if required, found := keyHeader(patch); !found || required {
			t.Fatalf("patch with Required on create only: header found %t, required %t; want an accepted header", found, required)
		}
		checkProblems(t, "patch", patch)
	})

	t.Run("ST-SUP-RC30-OC01-T03 Get, put and delete carry no Idempotency-Key header", func(t *testing.T) {
		operations := todoOperations(t, todoIdempotencyContext(types.OpCreate, types.OpPatch))
		for _, name := range []string{"GET /api/todo/v1/todos", "GET /api/todo/v1/todos/{id}", "PUT /api/todo/v1/todos/{id}", "DELETE /api/todo/v1/todos/{id}"} {
			operation, ok := operations[name]
			if !ok {
				t.Fatalf("the document has no operation %s", name)
			}
			if _, found := keyHeader(operation); found {
				t.Errorf("%s carries the Idempotency-Key header", name)
			}
		}
		// Without the idempotency component in the service, no operation carries the header.
		ctx := todoIdempotencyContext(types.OpCreate, types.OpPatch)
		ctx.PeerNamespaces = nil
		for name, operation := range todoOperations(t, ctx) {
			if _, found := keyHeader(operation); found {
				t.Errorf("without the idempotency component, %s carries the Idempotency-Key header", name)
			}
		}
	})
}
