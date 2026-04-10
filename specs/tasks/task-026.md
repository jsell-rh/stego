# Task 026: OpenAPI Request Body Validation Middleware

**Spec Reference:** "Request Validation" (rest-crud spec)

**Status:** `complete`

**Review:** [specs/reviews/task-026.md](../reviews/task-026.md)

## Description

The rest-crud archetype spec defines `request_validation: openapi-schema` as a convention that validates request bodies against the generated OpenAPI spec at runtime. This task generates validation middleware in the rest-api component.

### What changes

**Core types (`internal/types/types.go`):**
- Add `RequestValidation string` to `Convention` struct.

**Archetype YAML (`registry/archetypes/rest-crud/archetype.yaml`):**
- Add `request_validation: openapi-schema` to conventions.

**rest-api generator:**

**OpenAPI spec loading:**
- Generate startup code that loads the generated OpenAPI spec (e.g. from embedded bytes or file path).

**Validation middleware:**
- Generate middleware that validates POST, PUT, PATCH, and upsert request bodies against the OpenAPI schema.
- Use `kin-openapi` (github.com/getkin/kin-openapi) or equivalent library for schema validation.
- Validation checks: required fields, type constraints, min_length/max_length, pattern, min/max, enum values.

**Error responses:**
- Validation failures return RFC 9457 Problem Details (from Task 023) with per-field `validation_errors`:
  ```json
  {
    "type": "https://api.hyperfleet.io/errors/validation-error",
    "title": "Validation Error",
    "status": 400,
    "detail": "Invalid ClusterSpec",
    "code": "HYPERFLEET-VAL-000",
    "validation_errors": [
      { "field": "spec.region", "message": "property 'region' is missing" },
      { "field": "spec.diskSize", "message": "number must be at least 10" }
    ]
  }
  ```

**When `request_validation` is not set:** no validation middleware generated (current behavior).

**Wiring:**
- Generated go.mod includes kin-openapi dependency.
- Middleware wired in router before handlers.

## Spec Excerpt

> When `request_validation: openapi-schema` is set in the archetype conventions, the `rest-api` component generates middleware that validates request bodies against the generated OpenAPI spec at runtime.
>
> Entity field constraints (min_length, max_length, pattern, min, max, required) are already encoded in the generated OpenAPI spec. The validation middleware enforces them at runtime without hand-coded validation functions per entity.

## Acceptance Criteria

1. `RequestValidation` added to `Convention` struct; `request_validation: openapi-schema` added to archetype YAML.
2. Generated code loads OpenAPI spec at startup.
3. Validation middleware validates POST/PUT/PATCH/upsert request bodies.
4. Validation failures return RFC 9457 with `validation_errors` array.
5. Entity field constraints (required, type, min/max, pattern, enum) are enforced.
6. When `request_validation` is not set, no middleware generated.
7. Generated go.mod includes kin-openapi dependency.
8. Tests cover validation pass/fail scenarios.
9. `go build ./cmd/stego` compiles.

## Task Completion

When done, update this file's Status to `ready-for-review` and list relevant commits below.

## Commits

- 2f6a6cd feat(task-026): add OpenAPI request body validation middleware
