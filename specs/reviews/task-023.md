# Review: Task 023 — RFC 9457 Problem Details Error Handling

## Round 1

- [x] [process-revision-complete] **Error code prefix derivation produces wrong result.** `deriveErrorPrefix` (`generator.go:801-804`) removes hyphens and uppercases, producing `HYPERFLEETAPI` from `hyperfleet-api`. The spec (rest-crud spec §Error Handling, spec.md §Error Handling) and task description both state `hyperfleet-api` → `HYPERFLEET`. The function's own doc comment says `"hyperfleet-api" → "HYPERFLEET"` but the implementation contradicts it. Tests (`generator_test.go:4706`) assert the wrong value (`HYPERFLEETAPI`). All generated error codes (NTF, VAL, AUT, AUZ, CNF, INT) carry the incorrect prefix.

- [x] [process-revision-complete] **`handleError` does not populate `trace_id` from OpenTelemetry span context.** The task description states: "Populates `trace_id` from OpenTelemetry span context when available." The spec's example JSON includes `"trace_id": "4bf92f3577b34da6a3ce929d0e0e4736"`. The `TraceID` field exists on `ServiceError` (`generator.go:830`) but `handleError` (`generator.go:946-952`) never sets it. The generated function populates `Instance` and `Timestamp` but skips `TraceID`.

## Round 2

- [x] [process-revision-complete] **Slot rejection error responses produce empty `Code` field.** When a gate slot rejects a request (`!slotResult.Ok`), `emitBeforeSlot` (`generator.go:2084`) constructs an inline `ServiceError` without setting the `Code` field: `&ServiceError{Type: "about:blank", Title: http.StatusText(sc), Status: sc, Detail: slotResult.ErrorMessage}`. Since the `Code` JSON tag is `json:"code"` (no `omitempty`), the serialized response includes `"code": ""`. The rest-crud spec (§Error Handling) states all error responses carry structured error codes in `{SERVICE_PREFIX}-{CATEGORY}-{NUMBER}` format. The prefix is known at generation time; the runtime status code maps to a category (403→AUZ, 400→VAL, 401→AUT, 404→NTF, 409→CNF, default→INT). The error constructors all set `Code` correctly — only this inline construction path skips it.

- [x] [process-revision-complete] **Read handler classifies all storage errors as 404 Not Found.** `generateReadMethod` (`generator.go:537-538`) calls `NotFound()` for any error returned by `h.store.Get()`. A database connectivity failure or serialization error from the storage layer would produce a 404 Problem Details response with code `{PREFIX}-NTF-001` instead of a 500 Internal Server Error with code `{PREFIX}-INT-001`. The task description states "Not-found, validation, conflict, and internal errors all produce Problem Details responses" — implying each error type should use the appropriate constructor. The same pattern exists in the pre-task code (`http.Error(w, err.Error(), http.StatusNotFound)`), but the task's introduction of typed error constructors makes the misclassification explicit: the generated code now affirmatively labels an internal error as "Not Found" via the `NotFound()` constructor.

## Round 3

No findings. All acceptance criteria verified:

1. `ServiceError` type generated with all RFC 9457 fields (Type, Title, Status, Detail, Code, Instance, TraceID, Timestamp, ValidationErrors). ✓
2. Error constructors for all six categories (VAL, AUT, AUZ, NTF, CNF, INT) with correct codes. ✓
3. Error code prefix correctly derived from service name (`hyperfleet-api` → `HYPERFLEET`). ✓
4. `handleError` writes `application/problem+json` and populates Instance, Timestamp, TraceID. ✓
5. `error_type_base` produces URI-based Type field; absent uses `about:blank`. ✓
6. All handlers use `handleError` — no raw `http.Error` calls remain. ✓
7. Validation errors include per-field `validation_errors` array. ✓
8. Tests verify error response format, compilation, error classification, slot rejection codes, and error_type_base behavior. ✓
9. `go build ./cmd/stego` compiles. ✓

All 4 prior findings (rounds 1–2) confirmed fixed:
- Error prefix derivation: now drops last segment (`HYPERFLEET`, not `HYPERFLEETAPI`). ✓
- trace_id: populated from W3C Traceparent header. ✓
- Slot rejection Code field: uses `errorForStatus()` which sets Code. ✓
- Read error classification: uses `errors.Is(err, ErrNotFound)` to distinguish 404 from 500. ✓
