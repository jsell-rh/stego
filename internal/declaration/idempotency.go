package declaration

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/jsell-rh/stego/internal/types"
)

// operationMethods gives the HTTP method that serves each collection operation. A request with another method
// than POST or PATCH ignores the Idempotency-Key: GET reads, and PUT and DELETE are idempotent by their method.
var operationMethods = map[types.Operation]string{
	types.OpCreate: "POST",
	types.OpRead:   "GET",
	types.OpList:   "GET",
	types.OpUpdate: "PUT",
	types.OpUpsert: "PUT",
	types.OpDelete: "DELETE",
	types.OpPatch:  "PATCH",
}

// keyMethods are the HTTP methods whose requests carry the Idempotency-Key.
var keyMethods = []string{"POST", "PATCH"}

// keyOperations gives the operations that may require the key, which are the operations served by POST or
// PATCH, in name order.
func keyOperations() []string {
	var names []string
	for operation, method := range operationMethods {
		if slices.Contains(keyMethods, method) {
			names = append(names, string(operation))
		}
	}
	sort.Strings(names)
	return names
}

// IdempotencyOnIgnoredMethod is the specification ST-DECL-SP08: the declaration's idempotency setting names in
// Required on an operation that GET, PUT or DELETE serves, which ignore the key.
func IdempotencyOnIgnoredMethod(service *types.ServiceDeclaration) bool {
	if service == nil || service.Idempotency == nil {
		return false
	}
	for _, operation := range service.Idempotency.RequiredOn {
		if method, known := operationMethods[operation]; known && !slices.Contains(keyMethods, method) {
			return true
		}
	}
	return false
}

// validateIdempotency checks the idempotency setting (ST-DECL-INV25): Required on includes create, and names only
// operations served by POST or PATCH. Each fault names the operation, the method that serves it and the
// operations that may require the key, so that the author knows what to change.
func validateIdempotency(service *types.ServiceDeclaration) []ValidationError {
	if service == nil || service.Idempotency == nil {
		return nil
	}
	const path = "idempotency.required_on"
	var faults []ValidationError
	if !slices.Contains(service.Idempotency.RequiredOn, types.OpCreate) {
		faults = append(faults, ValidationError{
			Category: "idempotency",
			Rule:     "idempotency-required-on",
			Path:     path,
			Found:    operationList(service.Idempotency.RequiredOn),
			Allowed:  keyOperations(),
			Message:  "idempotency Required on does not name create: create is required, so that a retried create replays the stored response and does not create twice",
		})
	}
	for i, operation := range service.Idempotency.RequiredOn {
		method, known := operationMethods[operation]
		if known && slices.Contains(keyMethods, method) {
			continue
		}
		fault := ValidationError{
			Category: "idempotency",
			Rule:     "idempotency-required-on",
			Path:     fmt.Sprintf("%s[%d]", path, i),
			Found:    string(operation),
			Allowed:  keyOperations(),
		}
		if known {
			fault.Message = fmt.Sprintf("idempotency Required on names %q, which %s serves, and %s ignores the key: name only operations served by POST or PATCH", operation, method, method)
		} else {
			fault.Message = fmt.Sprintf("idempotency Required on names %q, which is no operation served by POST or PATCH", operation)
		}
		faults = append(faults, fault)
	}
	return faults
}

func operationList(operations []types.Operation) string {
	names := make([]string, len(operations))
	for i, operation := range operations {
		names[i] = string(operation)
	}
	return strings.Join(names, ", ")
}
