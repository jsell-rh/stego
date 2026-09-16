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
