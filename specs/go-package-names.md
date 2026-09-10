STEGO checks Go import paths and library package names before rendering. A
canonical filesystem path alone is not sufficient. The original Hypershell
probe changed the controller namespace to `bad-name`. Validation passed, but
planning failed when the generated package declaration was formatted.

The common `gen` checks distinguish three properties:

- `ValidateGoImportNamespace` checks a canonical path with Go's import-path
  rules. It rejects spaces, unsupported characters, and unsafe path forms.
- `ValidateGoLibraryName` checks a derived package identifier. It rejects
  keywords, the blank identifier, and `main`. A main package is a program and
  cannot serve as an imported library.
- `ValidateGoPackageNamespace` applies both checks when a generator uses the
  last directory segment as its library package name.

The checks apply to 12 library generators, including the legacy SSO generator.
The CLI namespace is a directory that contains packages with fixed names.
It uses the import-path check, so `cli-tools` remains valid. A controller can
use `go-services/worker`, but cannot use `go-services/bad-name`. The compiler
also checks the output directory as an import path. Application factories use
import-path checks because the bridge imports them with an explicit alias.
No path or package name is silently changed by these checks.

Protobuf requires a separate check after its Go mapping is resolved. Its Go
package name can differ from its directory. A regression found that `main/api.proto`,
`bad space/api.proto`, and a Unicode import directory passed preflight but
produced packages that Go could not import. Preflight now checks the actual Go
import path and package name selected by the protobuf generator. Existing
protobuf mappings for directories such as `sample-tools` and `type` still work.

Tests require validate, plan, and apply to reject invalid namespaces without
changing existing source, output, state, or dependencies. Separate tests cover
all 12 library generators, valid CLI containers, output directories, and factory
imports. Build tests import generated controller libraries from nested paths.
They also import generated protobuf packages from root, hyphenated, and keyword
directories. Names such as `init` remain available with an explicit import alias.

The Hypershell probe used application `25c09dd53cb36be90ddaf17945e8b3527f50a5e6`.
All three commands rejected controller namespaces `bad-name`, `workers/type`,
`workers/main`, and `workers/café`. Validation and planning accepted
`go-services/controller` and the original declaration. All output, state, and
dependency hashes remained unchanged. The nested Hypershell probe checks planning;
the independent generated-library builds supply compilation evidence. Moving an
application's actual imports after a namespace change remains application work.

These checks resolve the recorded namespace mismatch. C1 remains active until
the complete component, assembly, and command consistency audit is finished.
They do not establish the broader enterprise or application acceptance gates.

The final full `go test -race -count=1 -mod=readonly ./...` run passed with
PostgreSQL required on port 32901. The compiler package completed in 35.406
seconds and the gRPC generator package in 33.649 seconds. Vet passed. The earlier
controller command regression failed for all seven invalid namespace cases;
the protobuf regression separately failed for all three invalid mappings before
the checks were added.
