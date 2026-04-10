# Task 024: Response Envelope Format with Pagination

**Spec Reference:** "Response Format" (rest-crud spec)

**Status:** `ready-for-review`

**Review:** [specs/reviews/task-024.md](../reviews/task-024.md)

## Description

The rest-crud archetype spec defines `response_format: envelope` as a convention that wraps all responses with metadata. This task implements the envelope format in the rest-api generator, including single-resource presenters and list pagination.

### What changes

**Core types (`internal/types/types.go`):**
- Add `ResponseFormat string` to `Convention` struct.

**Archetype YAML (`registry/archetypes/rest-crud/archetype.yaml`):**
- Add `response_format: envelope` to conventions.

**Component YAML (`registry/components/postgres-adapter/component.yaml`):**
- Update version from `1.4.0` to `2.0.0` to reflect the GORM migration (Task 022).

**rest-api generator (`internal/generator/restapi/`):**

**Single resource responses:**
- Generate a presenter function per entity that adds `id`, `kind`, and `href` to the response.
- `id` — auto-generated UUID, assigned on create.
- `kind` — derived from entity name (e.g. `"Cluster"`).
- `href` — `base_path` + collection path + `id`.
- Create and read handlers return the presented entity.

**List responses:**
- Wrap list results in pagination envelope:
  ```json
  {
    "kind": "ClusterList",
    "page": 1,
    "size": 10,
    "total": 42,
    "items": [...]
  }
  ```
- `kind` — entity name + "List".
- `page` — requested page number.
- `size` — actual number of items returned.
- `total` — total count of matching records.

**List query parameters:**
- `page` — 1-indexed page number (default: 1).
- `size` — items per page (default: 100, max: 65500).
- `orderBy` — comma-separated, each entry is `field_name` or `field_name asc|desc` (default: asc).
- `fields` — sparse fieldset selection, comma-separated field names (`id` always included).

**Storage interface changes:**
- Add `ListOptions` struct: `Page int`, `Size int`, `OrderBy []OrderByField`, `Fields []string`.
- Add `ListResult` struct: `Items []T`, `Total int64`.
- List DAO functions accept `ListOptions` and return `ListResult`.

**Pagination mechanics:**
- Count total matching records first (`SELECT COUNT(*)`).
- Fetch page via `OFFSET (page-1)*size LIMIT size`.
- `orderBy` field names validated against entity fields; invalid fields rejected with 400.
- SQL injection prevented by field name validation + hardcoded direction strings.
- `size` capped at 65500; values above silently clamped.

**When `response_format` is not set or `bare`:** entities returned as plain JSON without wrapping or pagination (current behavior preserved).

## Spec Excerpt

> When `response_format: envelope` is set in the archetype conventions, the `rest-api` component wraps all responses:
>
> **Single resource responses** include `id`, `kind`, and `href` metadata.
>
> **List responses** wrap items in a pagination envelope:
> ```json
> {
>   "kind": "ClusterList",
>   "page": 1,
>   "size": 10,
>   "total": 42,
>   "items": [...]
> }
> ```

## Acceptance Criteria

1. `ResponseFormat` added to `Convention` struct; `response_format: envelope` added to archetype YAML.
2. Single resource responses include `id`, `kind`, `href` when envelope is enabled.
3. List responses include `kind`, `page`, `size`, `total`, `items` envelope.
4. List query parameters parsed and validated: `page`, `size`, `orderBy`, `fields`.
5. `size` capped at 65500.
6. `orderBy` field names validated against entity fields.
7. Storage interface includes `ListOptions` and `ListResult` types.
8. `response_format: bare` (or unset) preserves current behavior.
9. Tests cover envelope formatting, pagination, and query parameter validation.
10. `go build ./cmd/stego` compiles.

## Task Completion

When done, update this file's Status to `ready-for-review` and list relevant commits below.

## Commits
- 72693c9 feat(task-024): add ResponseFormat to Convention, update configs
- cfd01bd feat(task-024): implement response envelope format with pagination
- 68c8250 test(task-024): add tests for envelope format, pagination, and query params
- 9b49ee5 chore(task-024): regenerate example output with envelope format
- 8b23e1e chore(task-024): mark task ready-for-review
- f8a2edc fix(task-024): address round 1 review findings — href resolution, list presentation, OpenAPI completeness
- 4acd44e chore(task-024): regenerate example output with round 1 fixes
- c6463e9 chore(task-024): mark task ready-for-review after round 1 fixes
- ecea349 fix(task-024): address round 2 review findings — fields id, update ID, OpenAPI envelope
- 4c25767 chore(task-024): mark task ready-for-review after round 2 fixes
- ac4d859 fix(task-024): address round 3 review findings — fields validation, OpenAPI list items schema
- 36e4398 chore(task-024): mark task ready-for-review after round 3 fixes
- c0c1814 fix(task-024): address round 4 — sparse fieldset filters non-selected fields from response
