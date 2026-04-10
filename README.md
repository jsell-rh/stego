# STEGO

Service Templates, Extensible Generation & Operations.

STEGO is a declarative code generator that eliminates accidental complexity
from service development. You describe what your service is in a YAML
declaration; STEGO deterministically generates production-ready code from
trusted, pre-built components.

## Why

LLMs are increasingly writing code. But code involves two kinds of
complexity: essential (your business logic) and accidental (framework
wiring, boilerplate, conventions). LLMs make different accidental choices
every time. Across 100 services, that means 100 subtly different
implementations of the same patterns. Unreviewable. Ungreppable. Unknown
risk surface.

STEGO removes accidental complexity mechanically. Trusted components make
every accidental decision once. The only thing left in the YAML and in
the fills is essential complexity.

## How it works

1. Pick an **archetype** (e.g. `rest-crud`), a curated set of components
   that determines your architecture, conventions, and defaults.
2. Write a **service declaration** (`service.yaml`), your entities,
   operations, and slot bindings. This is the only file the LLM produces.
3. Write **fills** for business logic, Go functions implementing typed
   slot contracts (protobuf). Tested and qualified by a human.
4. Run `stego apply` for deterministic code generation. Plain Go output.
   No runtime dependency on STEGO.

## Prerequisites

- Go 1.24+ ([install](https://go.dev/dl/))

## Build from source

```bash
git clone https://github.com/jsell-rh/stego.git
cd stego
go build -o stego ./cmd/stego/
go test ./...
```
This produces a `stego` binary in the repo root.

## Quick start

Set up environment and initialize a new project:

```bash
export STEGO_REGISTRY=/path/to/stego/registry
mkdir my-service && cd my-service
/path/to/stego init -archetype rest-crud
```
This creates a `service.yaml` scaffold and a `fills/` directory. Edit
`service.yaml` with your entities and operations:

```yaml
kind: service
name: my-service
archetype: rest-crud
language: go
base_path: /api/my-svc/v1
error_type_base: https://api.example.com/errors/

entities:
  - name: Todo
    fields:
      - { name: title, type: string, min_length: 1, max_length: 255 }
      - { name: completed, type: bool, default: false }

collections:
  todos:
    entity: Todo
    operations: [create, read, update, delete, list]
```

Generate and build:

```bash
stego validate      # check service.yaml against registry
stego plan          # see what will be generated
stego apply         # generate code into out/
cd out && go build  # it's just Go
```

Add business logic via fills:

```bash
stego fill create admin-policy -slot before_create -collection todos
# implement fills/admin-policy/policy.go
stego test          # run fill tests
stego apply         # re-generate with fills wired in
```

## Run the example

A complete example with fills is included in `examples/user-management/`.
It demonstrates all rest-crud archetype features:

- **Collections** with multi-path access (org-scoped users + global user list)
- **base_path** for route prefixing (`/api/user-mgmt/v1`)
- **RFC 9457** error responses with `application/problem+json`
- **Envelope** response format with pagination (`page`, `size`, `total`, `items`)
- **Patch** operation with pointer-field request struct for partial updates
- **Upsert** with natural-key conflict resolution and optimistic concurrency
- **TSL search** integration (`?search=` on all list endpoints)
- **OpenAPI validation** middleware via kin-openapi
- **Slot wiring**: gate (RBAC policy), fan-out (notifications + audit), and
  short-circuit chain (org name validation + provisioning)

```bash
export STEGO_REGISTRY=/path/to/stego/registry
cd examples/user-management
/path/to/stego validate
/path/to/stego plan
/path/to/stego apply
cd out && go build
```

## Concepts

Six nouns: **Archetype**, **Component**, **Mixin**, **Service Declaration**, **Collection**, **Fill**.
Seven operators: `use`, `with`, `mixin`, `gate`, `chain`, `fan-out`, `map`.

See [specs/spec.md](specs/spec.md) for the full specification.

## License

Apache License 2.0. See [LICENSE](LICENSE).
