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
supply the pinned compiler and all build dependencies. Hypershell's current
generation scripts still fetch the compiler from Git. Registry vendoring alone
does not make those scripts work offline.

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

## Hypershell integration

Hypershell default branch `b21604b` pins all three modules to common registry
and compiler revision `db75a77da272fe05bbf6fdc3a3d1e0fede298f70`. Only two
application archetypes remain in its local registries. The complete public
workflow passed at `e8bb965`, and the complete CNPG dashboard workflow passed at
`aed33a9`. Both retained all three generation manifests and independent cleanup
evidence. See the [current application record](upstream-dashboard-integration.md).

Hypershell runtime revision `399a41b` uses this model for its API,
management console, and Gateway console. Each pins the common registry and
compiler to `8e0fae6f28276e192e8497c8acad487c765f3bb5`. Its local registries
contain two application archetypes and no common component declarations.
Application package paths are selected through `component_namespaces`.

The management UI in `components/web-console` and upstream dashboard inputs
in `components/gateway-dashboard` remain application inputs. The committed
`out`, `console/out`, and `gateway-console/out` directories remain generated
output. They are not alternate copies of the compiler or its templates.

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

These Hypershell changes are on remote `main` at `b21604b` and the development
branch. Earlier checkouts at `d6b1fe3` still contain the old local copies.
Generated runtime files remain committed for review and repeated generation.
