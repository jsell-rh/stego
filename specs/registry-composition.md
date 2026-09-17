# Registry composition

A consumer can use one through eight registry sources. STEGO captures all
sources before it validates or generates code. It combines distinct archetypes,
components, mixins, and protobuf inputs. It rejects repeated artifact names and
input paths, including identical copies. Source order does not permit a local
artifact to replace a common artifact.

Pin a Git source to its full commit SHA. Use `path` when the registry is a
subdirectory of that repository. Local URLs are relative to the project that
contains `.stego/config.yaml`.

```yaml
registry:
  - url: https://github.com/jsell-rh/stego.git
    ref: FULL_LOWERCASE_COMMIT_SHA
    path: registry
  - url: ./application-registry
    ref: application
```

Keep application archetypes and domain extensions in `application-registry`.
Give local artifacts distinct names. They can refer to components and protobuf
inputs from the common registry. Do not copy common declarations into that
local directory.

Use `component_namespaces` in `service.yaml` to select application output paths.
This changes the selected component's output path without changing its registry
declaration. STEGO rejects inactive component names, empty paths, invalid Go
package paths, compiler-owned paths, and overlapping output paths.

```yaml
component_namespaces:
  postgres-adapter: storage
  jwt-auth: auth
```

The common registry and compiler can have separate pins. A project can require
both pins to select the same reviewed STEGO revision. The input manifest records
its configuration file hash. State also records a reference hash for composed
sources and the hash of all captured registry YAML and protobuf inputs. A
change to any captured source after planning prevents apply.

Per-component `pins` are not supported. STEGO rejects nonempty entries before
it resolves registry sources. Earlier versions parsed these entries but did not
use them. Select revisions through each Git source's `ref`; do not use `pins`
to select or replace a common component.

Git sources use a verified local cache under `stego/registries` in the
operating system's user cache directory. On Linux, set `XDG_CACHE_HOME` to an
absolute writable directory when the home directory is read-only. This changes
the cache location; all pinned checkout checks still apply.

For an explicit offline
build, set `vendor` to a project-relative Git checkout that the build package
supplies. The checkout must contain its Git metadata and match the pinned
commit without modified, ignored, or untracked files. STEGO does not fetch the
remote or fall back to the cache when `vendor` is set. A plain directory without
Git metadata is not a verified vendored source.

This option supplies registry inputs only. A complete offline build must also
supply the pinned compiler, its verification inputs, the build tools, and all
build dependencies. Registry vendoring alone does not supply those inputs.

```yaml
registry:
  - url: https://github.com/jsell-rh/stego.git
    ref: FULL_LOWERCASE_COMMIT_SHA
    path: registry
    vendor: vendor/stego
  - url: ./application-registry
    ref: application
```

Registry subdirectories and vendor paths cannot contain symbolic links. The
combined input set retains the 64 MiB and 4,096-file limits. Each source also
has bounded directory depth and entry count. `STEGO_REGISTRY` remains an
explicit diagnostic override; it replaces the configured source set and emits
a warning.

## Project initialization

Create `.stego/config.yaml` before `stego init --archetype NAME` to select a
pinned common registry and local application extensions. Initialization uses
the same source resolver as generation. It keeps the configuration bytes,
including comments. Duplicate artifacts remain errors; source order does not
permit replacement. `STEGO_REGISTRY` keeps its explicit override behavior and
warning. If no configuration exists, initialization uses that override or
`./registry` and creates a local-source configuration.

Initialization captures and validates the registry before it writes project
files. It uses the same process lock as apply, then checks the captured inputs
and configuration again. It rejects existing `service.yaml` paths, symbolic
links, and invalid directory or configuration targets. Existing fills remain
unchanged. It publishes each new file from a complete, flushed temporary file
through an exclusive hard link. The filesystem must support hard links.
There is no fallback that can replace an existing file.

`service.yaml` is the last file published. This is not a transaction across all
files or a power-loss recovery protocol. A failed or interrupted attempt can
leave complete configuration files, empty directories, or a temporary file.
The next attempt can use a complete configuration. If `service.yaml` exists,
initialization stops and retains it. It reports only paths that it created.

### Initialization evidence

Source `cb4f0b9a7565563c9e28b1b9b06fe8490406408b` passed all six jobs in
[compiler run 35209395064](https://github.com/jsell-rh/stego/actions/runs/35209395064).
Independent log inspection confirmed all 13 selected initialization and registry
tests, plus the complete race suite with 34 passing packages. The other jobs
checked both examples, PostgreSQL provisioning, the real Keycloak provider,
and generated state storage. Compiler log SHA-256:
`3fa623f81cf6d5540a197ac65edf15c3f8c6eb8b9a5a3fdf378439de7699501c`.

[Artifact run 35209395056](https://github.com/jsell-rh/stego/actions/runs/35209395056)
built the same compiler twice with separate source trees and caches. Independent
inspection matched all 1,197 source files, the compiler checksum, the build
record, and the embedded source revision. Compiler SHA-256:
`31c6b61be34934541da20fa3ff5614b88c3647f15dc3c3b59449a2c8ee46870b`.
Build record SHA-256:
`a86d5f5f954bac5be9ea61d25586638f7bbcf14bfad70736dd520290740ab6ab`.
The branch artifact has no main signature and is not a consumer release.
No compiler build or Go test ran on the developer workstation.

The tested source is on remote main. Its main checks are tracked separately in
[run 35210141631](https://github.com/jsell-rh/stego/actions/runs/35210141631),
and its signed artifact check in
[run 35210141579](https://github.com/jsell-rh/stego/actions/runs/35210141579).
The signed artifact check passed. A separate local signature check verified
the main workflow, exact source, binary, and build record without executing
the compiler. Its bytes match the qualified branch artifact. The gate rejected
a different source commit, changed compiler bytes with updated checksums, a
changed build record, and an invalid signature bundle. Signature bundle SHA-256:
`36e833ab2d29fc8ee48817aaef00cd814a30975acf53c1caa2e62c7fcb5421ea`.
The main full-suite rerun is pending. Hypershell keeps its qualified compiler
pin while its live application checks finish.

## Hypershell integration

Hypershell `17f6295` removes the remaining local browser archetype. Both consoles
now use the common `browser-service`, including its telemetry component. Only
the API application archetype remains local. All three modules select compiler
and common registry `00573709fb15a2a54de4242aa8fdbabee325179a`.
The corrected full suite and public workflow passed at `af43205`. The public
run matched all 1,363 source hashes and 415 generated hashes; independent
cleanup passed. The corrected API run at `442e7ff` passed all 52 required
tests, with matching source and generation records and independent cleanup.
The CNPG run at `854bbb1` passed all 11 required tests in 766.91 seconds.
Independent checks matched all 1,368 source files and 415 generated hashes.
Six browser instances supplied all 48 startup log/span pairs with metrics and
no failed pairs. Database replacement, access, recovery, and durable deletion
passed. Independent cleanup confirmed that test runtime, allocations, volumes,
and the Lease holder were absent. One owned secondary Pod replacement was needed
for scheduling; the result does not prove fixture placement without intervention.
Hypershell `8813797` is on remote `main` with these records. See the
[current consumer record](https://github.com/jsell-rh/hypershell-stego/blob/8813797/acceptance/common-browser-composition.md).
The full enterprise requirements remain open.

### Earlier application checks

Hypershell source `0175b0b` pins all three modules to common registry
and compiler revision `83592bee5a17de6936cf521b94629e2a225a8d37`. Only two
application archetypes remain in its local registries. No common component
declarations remain there. The complete public workflow passed again at
`0175b0b` in [run 35188311004](https://github.com/jsell-rh/hypershell-stego/actions/runs/35188311004).
All 1,360 source hashes and 415 generated-file hashes matched. Its cleanup
check verified the completed workflow's resources while the next authorized
API test ran in its own namespace. The main API gate passed all 51 required
checks, and the core suite passed 307 top-level tests. The complete main CNPG
workflow passed all 11 required tests in
[run 35188311598](https://github.com/jsell-rh/hypershell-stego/actions/runs/35188311598).
All 415 generated-file hashes matched. Independent cleanup confirmed that its
resources and both observed volumes were absent. This run needed one secondary
database Pod replacement for scheduling; it does not prove fixture placement
without intervention. See the
[main CNPG evidence](https://github.com/jsell-rh/hypershell-stego/blob/9dabb6c/acceptance/main-cnpg-workflow-evidence.md).

The management UI in `components/web-console` and upstream dashboard inputs
in `components/gateway-dashboard` remain application inputs. The committed
`out`, `console/out`, and `gateway-console/out` directories remain generated
output. They are not alternate copies of the compiler or its templates.

## Earlier integration evidence

Hypershell runtime revision `399a41b` uses this model for its API,
management console, and Gateway console. Each pins the common registry and
compiler to `8e0fae6f28276e192e8497c8acad487c765f3bb5`. Its local registries
contain two application archetypes and no common component declarations.
Application package paths are selected through `component_namespaces`.

The [module check](https://github.com/jsell-rh/hypershell-stego/actions/runs/35161331243)
verified composed Git and local inputs, repeated generation, dependencies,
entry-point builds, and generated deployment checks at revision `b875ab2`.
All 121 archived source files matched the local module. Revision `399a41b`
selects that checked module and records its final dependency input hashes.
Common compiler revision `8e0fae6` passed
[all six CI jobs](https://github.com/jsell-rh/stego/actions/runs/35160694463).
These earlier results establish the registry integration. The later public
workflow at `e8bb965` establishes the deployed dashboard behavior described
above. The later expanded CNPG workflow is recorded in the
[integration record](upstream-dashboard-integration.md).

Earlier checkouts at `d6b1fe3` still contain the old local copies.
Generated runtime files remain committed for review and repeated generation.
