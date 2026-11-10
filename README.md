# STEGO

Service Templates, Extensible Generation & Operations.

STEGO generates common service and client code from YAML declarations and
reusable components. The output is plain Go, with no framework dependency.
Application fills supply domain rules. The requirements are the
Lathe model under `data/`; `lathe check` reports what is designed, built and
proved. Production use requires validation for the selected application and
deployment.
The Kubernetes service component (a retired record, in git history) supplies common
container build files, restricted Pod settings, network policy, and HTTPS probes.
Its workload policies do not cover application-allocated namespaces. The
namespace allocator (a retired record, in git history) has an optional deny-all
policy with admission protection. Allowed traffic and live network enforcement
for the Hypershell Gateway profile remain open; the application has not enabled
that policy.

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
2. Write a **service declaration** (`service.yaml`) with entities, operations,
   component settings, and slot bindings.
3. Write application code for domain rules. **Fills** implement typed slot
   contracts. The CLI component still takes a factory for its command
   definitions. Test each fill with `stego test`, and sign off its evidence
   with `stego fill sign`.
4. Run `stego apply` for deterministic Go and browser code generation.
   Validate, plan and apply stop on a bound fill that is not qualified.
   The output has no runtime dependency on STEGO.

A service whose archetype holds the command and query components, and whose
project has no record of a release on the GORM store, runs on the runtime data
path: its commands, its queries and its collection operations run on the
runtime library stego.dev/runtime. Its main reads the deployment settings from
the environment, and its binary with the one argument `migrate` is the
migration job, which makes the tables, the tables of the runtime library and the
authorization schema before the service starts.

The [Go SDK component](registry/components/go-sdk/README.md) generates typed
HTTPS clients from captured OpenAPI contracts. Its transport and optional
telemetry come from STEGO.

The [TypeScript SDK](registry/components/typescript-sdk/README.md) generates
same-origin browser clients, typed models, CSRF transport, and runtime contract
checks. Application code supplies its domain UI.

The [browser backend](registry/components/browser-backend/README.md) generates a
separate Go service for login, encrypted sessions, token renewal, sign-out,
static assets, and the API proxy. It uses the shared health and telemetry code.
Applications supply their pages and domain API. The real Hypershell protocol
check covers Gateway access and session recovery; a complete UI and production
browser deployment remain open.

The [CLI component](registry/components/cli-application/README.md) generates a
separate executable, command parsing, private configuration, and an HTTPS
client. Application code declares command names, paths, and fields. Common
code validates those declarations, binds path parameters, and reserves private
output files before credential requests. It also supplies OIDC browser and
device login, signed ID-token checks, and stored-session refresh. The component
tests use a Record
application. Hypershell tests exercise actual Gateway and service-account
workflows through the generated runtime.

## Prerequisites

- Go 1.26.10+ ([install](https://go.dev/dl/))

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
export PATH="/path/to/stego:$PATH"
mkdir my-service && cd my-service
stego init --archetype rest-crud
```

Consumers can compose a pinned common registry with local application
extensions (a retired record, in git history). Common component copies are not
required. Application output paths use `component_namespaces`; offline builds
can select an explicit verified vendored checkout.
Use the path of your STEGO checkout for `/path/to/stego`.
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
go build -o service ./out
```

Run the following STEGO commands from the project directory that contains
`service.yaml`. The build command keeps that working directory unchanged.

`stego validate` prints every fault of the declaration, or that there is none.
With `--json` it prints one JSON document for an agent: `accepted`, the
`faults` (each with its rule, path, value found, allowed values and message),
and the `settings` that checking resolved. Each setting has its `path`, its
`value` and `default`, which is `true` when checking filled the value in
because the declaration leaves it out, such as `idempotency.retention` with
`24h`, or `tenancy.kind` with `pooled` and `audit.level` with `change` for a
declaration that says nothing of tenancy or audit. A default that the
declaration leaves is a setting that is not a default, such as `tenancy.kind`
with `single`, and its reason is the setting `tenancy.reason` or
`audit.reason`. Each command gets `commands[i].retry_on_conflict` with `0` and
`commands[i].lock_timeout` with `2s` when it leaves them out, each slot
binding gets `slots[i].runs_as` with `tenant` when it does not run as system, and each
observation gets `observations[i].counts` with `attempt`. A declaration that
names no modes gets `modes` with `api`, `worker` and `controller`, and each worker
with an `every` trigger gets `workers[i].single` with `true` and, when it is
single, `workers[i].lease_ttl` with `15s`. Each schema change with a step gets
`schema_changes[i].phase`, the phase of its last step in `schema_history`:
`expand` while it is open, `contract` when it is finished.

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

Adapter version 4 requires verified database TLS by default. Configure
`sslmode=verify-full` and a trusted server certificate. The
database TLS contract (a retired record, in git history) describes private trust roots,
host fallbacks, and the explicit loopback test exception.

Set `DATABASE_URL` or `DATABASE_URL_FILE` for the service database. The file
setting supports mounted secrets with bounded reads and private file permissions.
See database secret sources (a retired record, in git history) for the source
rules and restart behavior.

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
stego fill create admin-policy --slot before_create --collection todos
stego apply         # generate the shared slot contracts
# implement fills/admin-policy/fill.go and add its tests
stego deps          # include dependencies used by the fill
stego test          # run fill tests and record their evidence
stego fill sign admin-policy   # sign off the evidence with your key
stego fill status   # show each bound fill and its qualification
# add the binding shown below to service.yaml, then apply again
stego apply
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

A fill must be qualified before a binding can use it. `stego test` runs the
fill tests and, when they pass, records the evidence in the fill's `fill.yaml`:
the digests of the fill's files and of its slot contract, and the compiler that
ran the tests. `stego fill sign <name>` signs that evidence with your own key,
which git config names (`user.signingkey`, with `gpg.format ssh` for an SSH
key). STEGO never makes or uses a key of its own. The project's `SIGNERS` file
lists the keys that may sign, in the SSH allowed signers format. A fill is
qualified when its evidence matches its current files and contract and a key
in `SIGNERS` signed it. A change to a fill's files or to its contract makes the
fill stale, so run `stego test` and `stego fill sign` again.

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
export PATH="/path/to/stego:$PATH"
cd /path/to/stego/examples/user-management
stego validate
stego plan
stego apply
stego deps
go build -o service ./out
```

## Commands

This table comes from the CLI command records of the spec. Do not edit it by
hand; see [Specification checks](#specification-checks).

<!-- stego:generated cli-commands -->
| Command | Usage |
|---|---|
| `stego apply` | `stego apply [--saved-plan] [--replace-hand-edits] [--base <ref>]` |
| `stego assets` | `stego assets --directory <folder> --output <zip>` |
| `stego build` | `stego build --source <repo> --revision <commit> [--module <path>] --target <path> --go <sdk>/bin/go --work <folder> --output <folder> --module-cache <folder>` |
| `stego build download` | `stego build download --source <repo> --revision <commit> [--module <path>] --target <path> --go <sdk>/bin/go --output <folder>` |
| `stego build verify` | `stego build verify --record <file> --record-sha256 <digest> --artifact <file>` |
| `stego build verify-source` | `stego build verify-source --record <file> --record-sha256 <digest> --source <folder>` |
| `stego deps` | `stego deps` |
| `stego drift` | `stego drift` |
| `stego fill create` | `stego fill create <name> --slot <slot> [--component <component>] [--collection <collection>]` |
| `stego fill sign` | `stego fill sign <name>` |
| `stego fill status` | `stego fill status` |
| `stego fleet` | `stego fleet --digest <digest> <checkout>...` |
| `stego fleet measures` | `stego fleet measures [--json] <checkout>...` |
| `stego fleet vex` | `stego fleet vex --registry <folder> <checkout>...` |
| `stego image build` | `stego image build (--build-record <file> --build-record-sha256 <digest> --application <file> \| --after <file> --build-result <folder>) --trust-store <file> --trust-store-sha256 <digest> [--entrypoint <name>] --output <folder>` |
| `stego image export` | `stego image export --record <file> --record-sha256 <digest> --image <folder> --build-record <file> --work <folder>` |
| `stego image publish` | `stego image publish (--record <file> --record-sha256 <digest> --image <folder> --build-record <file> \| --after <file> --image <folder>) --spdx-bundle <file> --cyclonedx-bundle <file> --signer-issuer <url> --signer-subject <identity> --gh <path> --source-repository <owner/name> --repository <repo> --registry-ca <file> --registry-ca-sha256 <digest> --credentials <file> [--token-origin <origin>]... [--blob-origin <origin>]... --work <folder>` |
| `stego image retrieve` | `stego image retrieve --record <file> --record-sha256 <digest> --build-record <file> --repository <repo> --registry-ca <file> --registry-ca-sha256 <digest> --credentials <file> [--token-origin <origin>]... [--blob-origin <origin>]... --work <folder> --spdx-bundle <file> --cyclonedx-bundle <file> --signer-issuer <url> --signer-subject <identity> --gh <path> --source-repository <owner/name>` |
| `stego image sign` | `stego image sign --image <folder> (--record-sha256 <digest> \| --after <file>) --spdx-bundle <file> --cyclonedx-bundle <file> --signer-issuer <url> --signer-subject <identity> --gh <path> --source-repository <owner/name>` |
| `stego image verify` | `stego image verify --record <file> --record-sha256 <digest> --image <folder> --build-record <file> --work <folder> --spdx-bundle <file> --cyclonedx-bundle <file> --signer-issuer <url> --signer-subject <identity> --gh <path> --source-repository <owner/name>` |
| `stego init` | `stego init --archetype <name>` |
| `stego lifecycle supersede` | `stego lifecycle supersede --registry <folder> (--component <name> \| --runtime <module>) --digest <digest> --successor <digest> [--successor-component <name>] --fails-after <YYYY-MM-DD> --needs-declaration-change yes\|no --needs-fill-change yes\|no --summary <text> [--reference <url>]...` |
| `stego lifecycle withdraw` | `stego lifecycle withdraw --registry <folder> (--component <name> \| --runtime <module>) --digest <digest> --fails-after <YYYY-MM-DD> --reason <text> [--reference <url>]...` |
| `stego plan` | `stego plan [--show] [--base <ref>]` |
| `stego recover` | `stego recover` |
| `stego registry compose` | `stego registry compose` |
| `stego registry consumers` | `stego registry consumers <component> <checkout>...` |
| `stego registry fills` | `stego registry fills --slot <slot> [--component <component>]` |
| `stego registry inspect` | `stego registry inspect <component> \| --archetype <name> [--json]` |
| `stego registry search` | `stego registry search [--provides <port>] [--requires <port>] [--slot <slot>]` |
| `stego registry sunsets` | `stego registry sunsets <checkout>...` |
| `stego test` | `stego test [--] [<test arguments>...]` |
| `stego validate` | `stego validate [--json]` |
| `stego version` | `stego version [--json]` |
<!-- stego:end -->

## Concepts

Six nouns: **Archetype**, **Component**, **Mixin**, **Service Declaration**, **Collection**, **Fill**.
Seven operators: `use`, `with`, `mixin`, `gate`, `chain`, `fan-out`, `map`.

The full specification is the Lathe model under `data/`; `npx lathe serve`
browses it. The prose specs are retired; each record that came from one cites it at the commit that last held it.

### Specification checks

`lathe check` loads the model and fails on any error: data that breaks the
model, a test file without the ID of its test record, a manual test in STEGO's
own spec, a component or runtime package no aggregate lists. The workflow
`spec.yml` runs it on every pull request with the Lathe commit it pins, which
moves together with the schema in `lathe.config.json`. Write the data only
through `lathe <aggregate> <command>` or the Lathe plugin's tools, never by
editing `data/*.jsonl`. For merges, enable Lathe's row-by-row driver once per
clone:

```sh
git config merge.lathe.driver './node_modules/.bin/lathe merge-data %O %A %B %P'
git config merge.lathe.name 'Lathe row-by-row merge of data/*.jsonl'
```

The user doc standard (ST-STD-DOCS) keeps this README current. The
docs it holds are the files that `internal/assure/testdata/user-docs.txt`
lists. Each `stego` command and flag in them must exist in the spec and be
built, each relative link must resolve, and each whole service declaration must
pass `stego validate`. The change gate refuses a change that alters a command
that a doc names, unless the same change updates that doc. The command table
above is generated; after a change to a CLI command record, write it again
and commit the result:

```sh
go test ./internal/assure -run TestOwnTree -update-docs
```

## License

Apache License 2.0. See [LICENSE](LICENSE).
