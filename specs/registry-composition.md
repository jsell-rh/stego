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

Hypershell development revision `f182ec5` uses this model for its API,
management console, and Gateway console. Each pins the common registry and
compiler to `42c7ea13fb95995e9d24666637bceb61dbdbd591`. Its local registries
contain two application archetypes and no common component declarations.
Application package paths are selected through `component_namespaces`.

The management UI in `components/web-console` and upstream dashboard inputs
in `components/gateway-dashboard` remain application inputs. The committed
`out`, `console/out`, and `gateway-console/out` directories remain generated
output. They are not alternate copies of the compiler or its templates.

The [module check](https://github.com/jsell-rh/hypershell-stego/actions/runs/35152843228)
verified composed Git and local inputs, repeated generation, dependencies,
entry-point builds, and generated deployment checks at revision `1547331`.
All 83 archived source files matched the local module. Revision `f182ec5`
selects that checked module. Common compiler revision `42c7ea1` passed
[all six CI jobs](https://github.com/jsell-rh/stego/actions/runs/35152146945).
These results establish the registry integration. They do not establish a
complete deployed dashboard. That application gate remains open in the
[integration record](upstream-dashboard-integration.md).
