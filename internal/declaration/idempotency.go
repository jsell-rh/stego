package declaration

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

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

// DefaultIdempotencyRetention is how long a key and its stored response are kept when the declaration gives no
// retention (ST-DECL-UC07-R03), the default of the runtime (ST-SUP-RP07-LC07).
const DefaultIdempotencyRetention = "24h"

// DefaultIdempotencyFingerprint is the fingerprint when the declaration gives none: the method, the route and the
// body (ST-DECL-UC07-R04).
const DefaultIdempotencyFingerprint = "body"

// idempotencySettings gives the idempotency setting as checking resolves it: Required on as declared, and the
// retention and the fingerprint as declared or else filled in by their defaults, each default marked. A
// declaration with no idempotency setting has none.
func idempotencySettings(service *types.ServiceDeclaration) []Setting {
	if service == nil || service.Idempotency == nil {
		return nil
	}
	idempotency := service.Idempotency
	requiredOn := make([]string, len(idempotency.RequiredOn))
	for i, operation := range idempotency.RequiredOn {
		requiredOn[i] = string(operation)
	}
	resolved := func(path, value, fallback string) Setting {
		if value == "" {
			return Setting{Path: path, Value: fallback, Default: true}
		}
		return Setting{Path: path, Value: value}
	}
	return []Setting{
		{Path: "idempotency.required_on", Value: requiredOn},
		resolved("idempotency.retention", idempotency.Retention, DefaultIdempotencyRetention),
		resolved("idempotency.fingerprint", idempotency.Fingerprint, DefaultIdempotencyFingerprint),
	}
}

// idempotencyFingerprints are the values Fingerprint may take: only body, the fingerprint over the method, the
// route and the canonical body that the runtime computes (ST-SUP-RP07-LC05).
func idempotencyFingerprints() []string { return []string{"body"} }

// maxIdempotencyRetention is the longest retention the runtime can keep a key for: Wrap adds the retention to the
// completion time as a Go duration, and Purge deletes the row when that time has passed (ST-SUP-RP07-LC07). A
// longer retention does not parse as a duration.
const maxIdempotencyRetention = time.Duration(1<<63 - 1)

// validateIdempotencyValues checks the values of the idempotency setting (ST-DECL-INV82): Fingerprint, when given,
// is body, and Retention, when given, is a Go duration greater than zero that the runtime can hold, such as 24h or
// 48h. Each fault names the value found and what is allowed there.
func validateIdempotencyValues(service *types.ServiceDeclaration) []ValidationError {
	if service == nil || service.Idempotency == nil {
		return nil
	}
	var faults []ValidationError
	if fingerprint := service.Idempotency.Fingerprint; fingerprint != "" && !slices.Contains(idempotencyFingerprints(), fingerprint) {
		faults = append(faults, ValidationError{
			Category: "idempotency",
			Rule:     "idempotency-value",
			Path:     "idempotency.fingerprint",
			Found:    fingerprint,
			Allowed:  idempotencyFingerprints(),
			Message:  fmt.Sprintf("idempotency Fingerprint %q is unknown: the only fingerprint is body, which covers the method, the route and the body", fingerprint),
		})
	}
	if retention := service.Idempotency.Retention; retention != "" {
		duration, err := time.ParseDuration(retention)
		if err != nil || duration <= 0 {
			faults = append(faults, ValidationError{
				Category: "idempotency",
				Rule:     "idempotency-value",
				Path:     "idempotency.retention",
				Found:    retention,
				Message: fmt.Sprintf("idempotency Retention %q is not a duration greater than zero and at most %s: give it in hours, minutes or seconds, such as 24h, 48h or 90m",
					retention, maxIdempotencyRetention.Truncate(time.Hour)),
			})
		}
	}
	return faults
}
