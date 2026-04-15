# Task 061: OpenAPI and Metadata Discovery Endpoint Generation

**Spec Reference:** `specs/registry/archetypes/rest-crud/spec.md` — **OpenAPI and Metadata Endpoints** section (lines 669–700)

**Status:** `not-started`

**Depends on:** task-009, task-024

## Description

The rest-crud spec defines three unauthenticated discovery endpoints that the `rest-api` component must generate automatically. No `service.yaml` configuration is needed — they are part of the archetype convention.

### 1. OpenAPI spec endpoint — `GET {base_path}/openapi`

- Returns the generated OpenAPI 3.0 JSON spec
- Content-Type: `application/json`
- The spec is the same one embedded for request validation, served at runtime so clients and tooling can discover the API contract

### 2. OpenAPI UI endpoint — `GET {base_path}/openapi.html`

- Returns a minimal HTML page that renders the spec using a CDN-hosted Swagger UI or Redoc
- Content-Type: `text/html`
- No build-time dependencies — the HTML references the CDN script and points to the `/openapi` endpoint

### 3. Metadata endpoint — `GET {base_path}`

- Returns service metadata as JSON
- Content-Type: `application/json`
- Response shape:
```json
{
  "kind": "API",
  "id": "hyperfleet-api",
  "href": "/api/hyperfleet/v1",
  "collections": [
    { "kind": "ClusterList", "href": "/api/hyperfleet/v1/clusters" },
    { "kind": "NodePoolList", "href": "/api/hyperfleet/v1/nodepools" }
  ]
}
```
- `id` is the service name from the declaration
- `collections` lists all top-level (unscoped) collections with their list kind and href
- Scoped collections (e.g. cluster-nodepools) are discoverable from the parent resource's `href`, not listed at the top level

### Implementation scope

- **rest-api generator** (`internal/generator/restapi/generator.go`): generate handler functions for all three endpoints
- **Assembler** (`internal/compiler/assembler.go`): register the three routes as unauthenticated (outside the auth middleware chain)
- **Tests**: verify the generator produces the correct handler code and route registrations

### All three endpoints are unauthenticated

These are documentation/discovery endpoints. They must be registered outside the auth middleware wrapper so they are accessible without a JWT.

## Acceptance Criteria

1. rest-api generator produces handler code for `GET {base_path}/openapi` serving OpenAPI JSON
2. rest-api generator produces handler code for `GET {base_path}/openapi.html` serving Swagger UI/Redoc HTML
3. rest-api generator produces handler code for `GET {base_path}` serving metadata JSON
4. Metadata endpoint lists only top-level (unscoped) collections
5. All three routes are registered as unauthenticated
6. Generator tests cover the new endpoint generation
7. `go test ./...` passes

## Task Completion

When done, update this file's Status to `ready-for-review` and list relevant commits below.

## Commits

