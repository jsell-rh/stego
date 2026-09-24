# Application build records

The native build and source checks are included in published compiler
`c92f591`. The generated example and Hypershell API passed native, source, image,
real signature, and private TLS registry checks. The complete live Hypershell
browser workflow also passed with seven signed images in run `35512430605`.
Offline module inputs are implemented and checked in CI. `stego build download`
produces a module download cache for one source revision. The cache holds
only `cache/download` zips and a canonical record. `stego build
--module-cache` builds offline from it: each build round extracts the zips
again under `go.sum` enforcement, so a changed zip member fails module
content verification even when the cache record is forged, and a missing
module fails the offline download check. The build record then carries
`dependency_proxy: "off"` and the cache inventory. Production trust
selection remains open, as does the rest of C3. See the
[current delivery evidence](application-delivery-evidence.json).

`stego build` selects one Git commit, module, and entry point. It checks the
complete pinned Go SDK before execution. It creates two private source trees,
module caches, and build caches. Each tree comes from the selected Git archive.
Each extracted file must match the raw Git tree, including its path, content,
and executable bit. Export attributes that change or omit inputs cause failure.
Links, special files, submodules, path escapes, and excessive input sizes fail.
The work and result directories must be new and outside the input repository.

The command checks generated files and generation inputs against saved state.
It downloads modules through the fixed public proxy, then disables module
downloads for compilation. Go settings and caches do not come from the caller.
Local module replacements must remain inside the recorded source snapshot.
The command records each compiled module, its content checksums, and any local
replacement path. The record must match the module list in the executable.
Unused module graph entries cannot stand in for compiled source. Module download
must preserve the recorded source, including `go.mod` and `go.sum`. The command
disables CGO, workspace selection, automatic toolchain selection, automatic PGO,
Go telemetry, and source stamping. It never runs the application executable.

Both builds must have identical executable bytes, source inventories, modules,
and Go build settings. The SDK and Git executable are checked again afterward.
The result directory contains `application`, `build.json`, and `SHA256SUMS`.
Failures retain the work directory for inspection. A changed source inventory
also produces a bounded list of changed paths and their before and after hashes. A partial result is not a
successful build. Do not reuse an interrupted work or result directory.

For example, in a limited Linux amd64 CI job:

```sh
stego build --source=/checkout --revision=FULL_COMMIT_ID \
  --module=console --target=out --go=/opt/go/bin/go \
  --work=/results/build-work --output=/results/application
```

`stego build verify` requires the expected build record digest from a trusted
source. It checks canonical record bytes, build policy, executable bytes, and
the executable's Go build settings and module identities. It does not execute the application.

```sh
stego build verify --record=/results/application/build.json \
  --artifact=/results/application/application --record-sha256=TRUSTED_DIGEST
```

`stego build verify-source` compares a complete source snapshot with that native
record. It uses the same strict record reader and source inventory as the build
command. It rejects changed, missing, or additional files, changed executable
bits, links, special files, and excessive file counts or bytes. The selected
record digest must come from authenticated provenance. No source is executed.

```sh
stego build verify-source --record=/results/application/build.json \
  --record-sha256=TRUSTED_DIGEST --source=/work/application
```

Supply a source snapshot without Git metadata, dependency caches, or other work
files. Run this check before a deployment job adds test dependencies. After
regeneration, remove completed compiler lock files before checking the complete
snapshot again. Keep the checked source unchanged while using the result. This
check does not freeze the source or replace image and signature verification.

The CI check exercises this command against the actual example and Hypershell
API source inputs, then changes file contents, file presence, execute permission,
and links. Each application passed all nine source verification cases in main
run `35510989870`. The native verifier passed ten cases for each application.

The caller must authenticate the record before it supplies that digest. Hash
agreement does not authenticate the builder or source. The command does not
provide an OS sandbox or prove that a compiler cannot read other host files.
Run it in a disposable CI environment with resource and time limits. It does
not yet record image contents or a trust store. Private dependency transport
policy, image assembly and verification, signing, and consumer integration
remain required parts of the common build workflow.

The focused CI check builds the committed generated example twice. A separate
CI job builds the pinned Hypershell API twice with the same common command.
Both jobs check changed executables, changed records, compiled modules, build
settings, and input inventories. The Hypershell source stays unchanged.
This is application evidence for the common mechanism. It does not replace the
complete Hypershell application workflow or the existing signed compiler check.

## Native application evidence

[Application build run 35506669775](https://github.com/jsell-rh/stego/actions/runs/35506669775)
used candidate `f7587160e4e3a8a3963c63f299fc38f0b64a37ec`. The separate API job
used unchanged Hypershell source `0589cfc08d40591e3fc0b36ac1538e6f2ae0d917`.
Both jobs passed. Independent artifact inspection checked executable and record
hashes, both captured source archives, every recorded input, and all ten
verification cases for each application. The common package passed 19 test
groups with 55 cases, including subtests. No group failed or was skipped.

The example record contains 1,327 source files and 53 modules. The Hypershell
record contains 1,615 source files and 49 modules. Module counts include the
main application. Both builds preserved their inputs and produced identical
executable bytes from separate source trees and caches. See the
[evidence record](application-build-records-evidence.json) for exact hashes.

The earlier checks exposed two separate SDK issues. The inventory encoding
needed the established ASCII filename format. After that correction, an
independent check still found a different runner SDK inventory. The build now
uses the verified official SDK; its pinned identity did not change. A later
build rejected source changes. The default module download and compiled-module
inventory passed while retaining that rejection check. Earlier failed runs
remain recorded; they are not passes.

This check did not execute the application or publish an image. It does not
replace the REST, gRPC, restart, or cleanup checks for the running Gateway
workflow. Application record authentication, image content and trust-store
verification, and use in the production publisher remain open.

[Compiler check run 35506669820](https://github.com/jsell-rh/stego/actions/runs/35506669820)
passed all six jobs. The saved example output and module files matched the
selected source: 74 files for user-management and 70 for user-management-rhsso.
[Compiler artifact run 35506675763](https://github.com/jsell-rh/stego/actions/runs/35506675763)
also passed. Its source inventory, executable, and record hashes passed the
independent check. This candidate artifact is not signed or released.
