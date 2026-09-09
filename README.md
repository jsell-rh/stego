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

- Go 1.26.8+ ([install](https://go.dev/dl/))

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
stego deps          # resolve and check project dependencies
cd out && go build  # it's just Go
```

Run `stego deps` after apply and after changes to application imports. It runs Go
module resolution, module verification, and package dependency checks. It uses
temporary module files, then saves `go.mod` and `go.sum` with transaction recovery.
Commit both files. A command failure or an input change stops the update.
The command has a five-minute limit. It checks up to 10,000 Go source and module
files, with a total size limit of 256 MiB. Local replacement directories must
exist. Their Go source and module files are checked for changes too.

Dependency resolution uses `GOWORK=off` and clears `GOFLAGS` for its Go commands.
Other Go environment settings, including private module and proxy settings,
remain in effect. Plan and apply do not resolve or download dependencies.
These checks do not replace application tests or dependency security review.

The PostgreSQL adapter prepares model metadata before concurrent work starts.
This step does not read or change database tables. It also runs when migrations
are external. Adapter version 3 changes `NewStore(db)` to return `(*Store, error)`.
Generated startup code checks that error before it starts listeners or tasks.
Update handwritten constructor calls to handle the error after regeneration.
Construct the store before other code uses its models on the same GORM connection.

The generated HTTP server uses a five-second header deadline, 30-second read and
write deadlines, a 60-second idle deadline, and a 32 KiB header limit setting.
SIGINT and SIGTERM start shutdown. Active requests have ten seconds to finish.
After that interval, the server closes remaining connections and reports an
error. Deferred resource cleanup runs before process exit. These defaults serve
the current request-response API. Long-lived streams need a separate deadline
policy. Network deadlines do not stop application code that ignores cancellation.

Before you start the service, set `STEGO_AUTH_ISSUER`, `STEGO_AUTH_AUDIENCE`, and
`STEGO_AUTH_PUBLIC_KEY_FILE`. The key file must contain one RSA public key in PEM
format. The default authentication component verifies RS256 signatures and
requires the configured issuer and audience, a subject, an issue time, and an
expiry. It accepts the `JWT` token type. Incomplete settings stop startup. Restart
the service after a public key change. Automatic key rotation is not yet supplied.

Add business logic via fills:

```bash
stego fill create admin-policy -slot before_create -collection todos
# add the binding shown below to service.yaml
stego apply         # generate the shared slot contracts
# implement fills/admin-policy/fill.go and add its tests
stego deps          # include dependencies used by the fill
stego test          # run fill tests
```

```yaml
slots:
  - collection: todos
    slot: before_create
    gate: [admin-policy]
```

New fill methods return an error until you implement them. They use the generated
slot types and include a constructor and an interface check. STEGO does not
replace existing fill files.

Apply saves a transaction record before it changes output. If apply stops before
completion, run `stego recover`. Recovery verifies the record and all affected
files before it completes the saved changes. A conflicting file stops recovery.
Plan and drift refuse to report complete output while a transaction is pending.
Source edits made after an interruption are preserved for the next plan.

Do not remove `.stego/apply.lock` while a STEGO process runs. The file remains
after normal use; the operating system releases its lock when the process stops.
One apply can change up to 64 MiB of file contents. Its saved transaction record
has a 128 MiB limit. Recovery tests cover process interruption on Linux. Other
programs can still observe separate file replacements during apply.

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
