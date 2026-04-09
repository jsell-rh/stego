# STEGO - Service Templates, Extensible Generation & Operations

A declarative framework for eliminating accidental complexity from service development. LLMs distill human intent into a service declaration; a compiler deterministically produces runnable code from trusted, pre-built components. No LLM touches the generated code.

Core insight (Brooks, "No Silver Bullet"): accidental complexity is the attack surface for probabilistic systems. Eliminate it mechanically so the LLM never has to choose.

## Glossary

Six nouns, seven operators.

| Term | What it is | Who owns it |
|------|-----------|-------------|
| **Archetype** | Curated component set + conventions. One per service. Determines architecture, layout, error handling, logging. | Platform team |
| **Component** | Deterministic code generator. Has config schema, ports (requires/provides), slots (typed extension points), and templates. | Platform team |
| **Mixin** | Adds components and slots to an archetype without changing its conventions. Additive only. | Platform team |
| **Service Declaration** | Selects archetype, declares entities and collections, binds slots. The only thing the LLM produces. Language-agnostic. | Product team / LLM |
| **Collection** | A scoped, operation-constrained access pattern over an entity. Multiple collections can reference the same entity. Each collection generates its own handler, routes, and wiring. | Product team / LLM |
| **Fill** | Code (e.g. Go function) implementing a slot's protobuf contract. Has tests. Qualified by a human. | Product team |

Operators: `use`, `with`, `mixin`, `gate`, `chain`, `fan-out`, `map`.

### Entity/Collection Separation

Entities define data (fields, types, constraints). Collections define access patterns (which entity, what scope, what operations, what URL). This separation is load-bearing:

- An entity is declared once. A collection references it.
- Multiple collections can reference the same entity with different scopes and operations.
- Each collection generates its own handler. The entity struct is shared.
- Slots bind to collections, not entities. Different access paths can have different business logic.
- Paths are derived from collection names and scopes, or declared explicitly via `path_prefix`.

This makes multi-path access the default case, not an exception. REST APIs project entity graphs onto URL trees; that projection is inherently 1:N.

## Slot/Fill Contract

Slots are defined as protobuf service definitions. Each component owns its own slot protos (no sharing between components -- duplication is cheaper than coupling). Fills implement the generated interface in the target language.

Common stable types (Identity, SlotResult) live in a shared `stego.common` proto package. The bar for inclusion is very high.

## File Types

### Platform team creates:

**Archetype** -- a curated component set with conventions. See [specs/registry/archetypes/rest-crud/spec.md](registry/archetypes/rest-crud/spec.md) for the `rest-crud` archetype specification.

**Component** -- a Go package implementing the `Generator` interface, with a config schema, ports, slots, and an output namespace. Each component also has `slots/*.proto` for slot contracts.

**Mixin** -- adds components and slots to an archetype without changing its conventions.

## Code Generation Mechanism

Components are Go packages implementing a `Generator` interface. Code is generated programmatically (`fmt.Fprintf` to a buffer + `go/format.Source()` for formatting), not via text templates. Zero external dependencies -- standard library only. This gives full conditional logic, type checking at compile time, and testability.

```go
type Generator interface {
    Generate(ctx gen.Context) ([]gen.File, error)
}
```

The language of the generator (Go) is independent of the language of the output. A Go generator can produce Go, Python, Rust -- the output is just strings. This is the same pattern as protobuf (`protoc` is C++, plugins generate any language).

Generators live in the registry as Go packages. The stego compiler imports and calls them.

## Port Resolution

Components declare `requires` and `provides` ports. Resolution uses archetype defaults with service-level overrides:

1. The archetype declares default bindings (`storage-adapter: postgres-adapter`)
2. The service declaration can override any binding via `overrides:`
3. Every resolved component is SHA-pinned in `.stego/state.yaml`

```yaml
# archetype declares defaults
bindings:
  storage-adapter: postgres-adapter
  auth-provider: jwt-auth

# service.yaml can override
overrides:
  auth-provider: api-key-auth

# .stego/config.yaml pins the override to exact SHA
pins:
  api-key-auth: c9d8e7f6a5b4
```

The compiler validates that every `requires` port has exactly one provider. Unresolved or ambiguous ports are a compile error. Full audit trail from generated code back to exact component SHAs.

## Entity Type System

Types and constraints are essential complexity -- they describe what the data IS, not how it's stored or validated. They belong in the IR. The component decides HOW to enforce them (SQL constraints, middleware, ORM annotations).

Primitive types (aligned with protobuf): `string`, `int32`, `int64`, `float`, `double`, `bool`, `bytes`, `timestamp`.

Stego-specific types: `enum`, `ref`, `jsonb`.

Constraint attributes (finite set, not extensible):
- `min_length`, `max_length`, `pattern` (strings)
- `min`, `max` (numerics)
- `unique`, `unique_composite: [field1, field2]`
- `optional`, `default`
- `values: [a, b, c]` (enums)
- `computed: true, filled_by: <fill>` (derived fields)

```yaml
fields:
  - { name: email, type: string, max_length: 255, unique: true }
  - { name: name, type: string, min_length: 3, max_length: 53, pattern: "^[a-z0-9]" }
  - { name: role, type: enum, values: [admin, member] }
  - { name: org_id, type: ref, to: Organization }
  - { name: metadata, type: jsonb, optional: true }
  - { name: status, type: jsonb, computed: true, filled_by: status-aggregator }
```

If a constraint can't be expressed with these attributes, it becomes a fill -- that's the boundary between "what the data is" and "domain logic."

## Stego is Written in Go

Stego itself is Go. Components are Go packages implementing `Generator`. The compiler imports and calls them directly -- the registry is a Go module. Single binary distribution. If multi-language generators are needed later, a subprocess protocol can be added without changing the architecture.

The `language` field on the service declaration must be validated against the archetype's declared language. If they disagree, it is a validation error. Only `go` is supported in MVP; other values are rejected.

## Migration Diffing

Migration generation is a component concern, not a stego concern. The compiler passes entity definitions (desired state) to the storage component's generator. The component owns the diffing strategy -- the `postgres-adapter` might use Atlas internally, `sqlite-adapter` might use something else. Stego doesn't know or care. This is consistent with the principle that components own accidental complexity.

## Fill Wiring

Fills are wired via constructor injection using generated interfaces. No DI framework, no reflection, no runtime lookup.

1. The compiler generates a Go interface from each slot's proto definition
2. Fills implement that interface
3. Generated `main.go` wires concrete fills into constructors

```go
// generated interface from slot proto
type BeforeCreateSlot interface {
    Evaluate(ctx context.Context, req *BeforeCreateRequest) (*SlotResult, error)
}

// generated main.go -- full dependency graph visible in one place
func main() {
    userHandler := api.NewUserHandler(
        userService,
        adminpolicy.New(),      // fill: gate
        usernotifier.New(),     // fill: fan-out
        auditlogger.New(),      // fill: fan-out
    )
}
```

An auditor reads `main.go` and sees every fill and every connection. Tests swap any fill for a mock by passing a different interface implementation.

## Generated Code Structure

Each component declares an `output_namespace` (e.g. `internal/api`) and can only write files under it -- the compiler rejects any file outside the namespace. The archetype validates at authoring time that no two component namespaces overlap (static YAML check).

Shared files (`cmd/main.go`, `go.mod`, `Dockerfile`, `openapi.yaml`) are owned by the compiler, not by any component. Each component's generator returns a `Wiring` struct (imports, constructors, routes) alongside its files, and the compiler assembles the shared files from all wiring declarations.

The archetype's conventions (e.g. `layout: flat` vs `layout: hexagonal`) are passed to generators via `gen.Context`, influencing how generators organize files within their namespace.

STEGO enforces only two rules:

1. All generated files carry a `// Code generated by stego. DO NOT EDIT.` header
2. `fills/` lives outside the generated output (human-owned, never overwritten)

```
project/
  service.yaml               # human/LLM authored
  fills/                     # human-owned code, never touched by stego
    admin-creation-policy/
    user-change-notifier/
  .stego/
    config.yaml
    state.yaml
  out/                       # generated -- layout determined by archetype
    cmd/main.go
    internal/...
    go.mod
    openapi.yaml
    Dockerfile
```

### Product team creates:

**Service declaration** (the only thing the LLM writes):
```yaml
# service.yaml
kind: service
name: user-management
archetype: rest-crud
language: go

entities:
  - name: Organization
    fields:
      - { name: name, type: string, unique: true }

  - name: User
    fields:
      - { name: email, type: string, unique: true }
      - { name: role, type: enum, values: [admin, member] }
      - { name: org_id, type: ref, to: Organization }

collections:
  organizations:
    entity: Organization
    operations: [create, read]

  org-users:
    entity: User
    scope: { org_id: Organization }
    operations: [create, read, update, list]

  all-users:
    entity: User
    operations: [list]

slots:
  - collection: org-users
    slot: before_create
    gate:
      - rbac-policy
      - admin-creation-policy

  - collection: org-users
    slot: on_entity_changed
    fan-out:
      - user-change-notifier
      - audit-logger

mixins:
  - event-publisher

# only if deviating from archetype defaults
overrides:
  jwt-auth:
    header: X-Internal-Token
```

**Fill:**
```yaml
# fills/admin-creation-policy/fill.yaml
kind: fill
name: admin-creation-policy
implements: rest-api.before_create
collection: org-users

qualified_by: jsell
qualified_at: 2026-04-06
```

```go
// fills/admin-creation-policy/policy.go
func (f *AdminCreationPolicy) Evaluate(
    ctx context.Context,
    req *slots.BeforeCreateRequest,
) (*slots.SlotResult, error) {
    if req.Input.Fields["role"] == "admin" && req.Caller.Role != "admin" {
        return &slots.SlotResult{Ok: false, ErrorMessage: "only admins can create admins"}, nil
    }
    return &slots.SlotResult{Ok: true}, nil
}
```

```go
// fills/admin-creation-policy/policy_test.go
func TestNonAdminCannotCreateAdmin(t *testing.T) { ... }
func TestAdminCanCreateAdmin(t *testing.T) { ... }
```

## CLI Interface

```
Project lifecycle:
  stego init --archetype <name>        Create project from archetype
  stego fill create <name> --slot <s>  Scaffold a new fill with generated interface

Reconciliation:
  stego plan                           Diff desired vs current, show changeset
  stego apply                          Generate/update code
  stego drift                          Detect hand-edits to generated files

Validation:
  stego validate                       Check service.yaml against registry
  stego test                           Run all fill tests

Registry:
  stego registry search                Query components by provides/requires/slots
  stego registry inspect <component>   Show component details
  stego registry fills --slot <s>      Find existing fills for a slot
```

No LLM integration. STEGO is purely the deterministic side.

## Registry

The registry is a git repo. No database, no server. Versions are git tags for discovery, but all resolution pins to SHAs for auditability.

```yaml
# .stego/config.yaml
registry:
  - url: git.corp.com/platform/stego-registry
    ref: a1b2c3d4e5f6  # pinned SHA, not a branch or tag

# per-component SHA pins override the registry ref
pins:
  rest-api: f4e5d6c7b8a9
  postgres-adapter: 3a2b1c0d
  # everything else resolves from registry ref
```

Resolution order: pinned SHA > registry ref. `stego plan` warns on stale pins.

Multiple registries supported (org-wide + team-specific, team takes precedence). Publishing = PR to the registry repo. Promoting a fill to a component = PR that adds it to the registry.

## Compiler Process (Reconciler Pattern)

Follows Terraform-style plan/apply, not one-shot generation:

```bash
$ stego plan

Changes detected in service.yaml:
  entities.User:
    + field: display_name (string)

Plan:
  generate: storage/migrations/002_add_display_name.sql
  update:   api/handlers_user.go
  update:   storage/models.go
  unchanged: 12 files

$ stego apply
```

State tracked in `.stego/state.yaml`, pinned to exact registry SHAs for full auditability:

```yaml
# .stego/state.yaml
last_applied:
  registry_sha: a1b2c3d4e5f6
  components:
    rest-api:
      version: 2.1.0
      sha: a1b2c3d4e5f6
    postgres-adapter:
      version: 1.4.0
      sha: a1b2c3d4e5f6
```

Drift detection via `stego drift`.

Output is plain Go (or target language). No runtime dependency on STEGO. `go build` works.

## Promotion Path

Fills that appear in 3+ projects get promoted to components (code generators with config schemas). Components commonly paired together become mixins. Mixins that define full architectural patterns become archetypes.

```
Fill (project-scoped code)
  -> Component (reusable code generator)
    -> Mixin (bundled components)
      -> Archetype (full architectural pattern)
```

Each promotion changes the artifact's kind -- a fill is code, a component is a code generator. Real work, not just relabeling.

## Brownfield Adoption

Existing services are not rewritten. They get wrapped as components that new services can consume:

```yaml
name: legacy-auth-client
wraps: git.corp.com/platform/auth-sdk
provides:
  - auth-middleware
```

New services only. Existing services become components in the registry.

## MVP Scope

Single archetype (`rest-crud`), end-to-end with fills and slots working. Full CLI.

**Archetype & Components:**
- `rest-crud` archetype
- `rest-api` component (handlers, routes, middleware, OpenAPI)
- `postgres-adapter` component (models, queries, migrations)
- `jwt-auth` component

**CLI Commands (all 11):**
- `stego init`, `stego plan`, `stego apply`, `stego drift`
- `stego validate`, `stego test`
- `stego fill create`
- `stego registry search`, `stego registry inspect`, `stego registry fills`

**Slot/Fill demonstration:**
- At least one gate fill (e.g. `admin-creation-policy`)
- At least one fan-out fill (e.g. `audit-logger`)
- Generated interfaces, constructor injection, wired `main.go`

**Example service:** simplified hyperfleet-api or similar, producing a compilable, runnable Go service from a single `service.yaml` + fills.

**Deferred to post-MVP:** multiple archetypes, mixins, multiple registries, per-component SHA pinning, multi-language output.

## Open Questions

- The first ~10 services will be blocked waiting for components that don't exist yet. Mitigation: seed the registry from existing real services; allow early services to be fill-heavy with TODOs to extract reusable components later

