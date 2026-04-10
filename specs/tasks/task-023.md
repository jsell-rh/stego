# Task 023: RFC 9457 Problem Details Error Handling

**Spec Reference:** "Error Handling (RFC 9457)" (rest-crud spec)

**Status:** `ready-for-review`

**Review:** [specs/reviews/task-023.md](../reviews/task-023.md)

## Description

The rest-crud archetype convention `error_handling: problem-details-rfc` is already declared in the archetype YAML but has no implementation. This task generates RFC 9457 Problem Details error handling in the rest-api component.

### What changes

**rest-api generator (`internal/generator/restapi/`):**

**ServiceError type:**
- Generate a `ServiceError` struct with RFC 9457 fields: `Type`, `Title`, `Status`, `Detail`, `Code`, `Instance`, `TraceID`, `Timestamp`.
- Optional `ValidationErrors []ValidationError` for validation failures (field + message pairs).

**Error constructors:**
- `NotFound(entityKind, id string)` → 404 with code `{PREFIX}-NTF-001`
- `BadRequest(detail string)` → 400 with code `{PREFIX}-VAL-001`
- `Conflict(detail string)` → 409 with code `{PREFIX}-CNF-001`
- `Validation(errors []ValidationError)` → 400 with code `{PREFIX}-VAL-000`
- `Unauthorized(detail string)` → 401 with code `{PREFIX}-AUT-001`
- `Forbidden(detail string)` → 403 with code `{PREFIX}-AUZ-001`
- `InternalError(detail string)` → 500 with code `{PREFIX}-INT-001`

**Error code prefix:** derived from service name (uppercased, hyphens removed): `hyperfleet-api` → `HYPERFLEET`.

**handleError function:**
- Serializes `ServiceError` as JSON with `Content-Type: application/problem+json`.
- Populates `instance` from the request path.
- Populates `trace_id` from OpenTelemetry span context when available.
- Populates `timestamp` with current UTC time.

**error_type_base support:**
- When `error_type_base` is set on the service declaration (from Task 021), the `type` field uses it as a prefix (e.g. `https://api.hyperfleet.io/errors/not-found`).
- When not set, `type` uses a default pattern (e.g. `about:blank` per RFC 9457).

**All generated handlers use handleError:**
- Replace raw `http.Error()` calls with `handleError()`.
- Not-found, validation, conflict, and internal errors all produce Problem Details responses.

## Spec Excerpt

> ```json
> {
>   "type": "https://api.hyperfleet.io/errors/not-found",
>   "title": "Not Found",
>   "status": 404,
>   "detail": "Cluster with id 'abc123' not found",
>   "code": "HYPERFLEET-NTF-001",
>   "instance": "/api/hyperfleet/v1/clusters/abc123",
>   "trace_id": "4bf92f3577b34da6a3ce929d0e0e4736",
>   "timestamp": "2026-04-09T12:00:00Z"
> }
> ```

## Acceptance Criteria

1. `ServiceError` type generated with all RFC 9457 fields.
2. Error constructors generated for all six categories (VAL, AUT, AUZ, NTF, CNF, INT).
3. Error code prefix derived from service name.
4. `handleError` writes `application/problem+json` responses.
5. `error_type_base` populates the `type` URI when set.
6. All handlers use `handleError` instead of raw `http.Error`.
7. Validation errors include per-field details array.
8. Tests verify error response format.
9. `go build ./cmd/stego` compiles.

## Task Completion

When done, update this file's Status to `ready-for-review` and list relevant commits below.

## Commits

- 8bd6c1d feat(task-023): generate RFC 9457 Problem Details error handling
- 39db311 fix(task-023): correct error prefix derivation and add trace_id population
