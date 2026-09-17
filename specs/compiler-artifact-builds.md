# Compiler artifact build check

The `Compiler artifact check` workflow builds the Linux amd64 compiler twice
from one exact Git commit. Each build uses a separate source tree, home,
temporary directory, module cache, and build cache. It compares the executable
bytes, module checksum records, and compiler version records before it saves
an artifact. The first CI result is recorded below.

The check uses Go 1.26.8, `CGO_ENABLED=0`, `GOAMD64=v1`, `-trimpath`,
`-buildvcs=true`, and `-mod=readonly`. It disables workspace files, user Go
configuration, inherited flags, toolchain switching, and external build-cache
programs. It supplies a fixed environment instead of inheriting credentials,
linker settings, compiler settings, or Git configuration from the runner.
Go telemetry is disabled in each isolated home.

Dependency preparation uses the public Go module proxy and checksum database.
Local module replacements are rejected. The compiler build then runs with
`GOPROXY=off`. Module verification runs before and after compilation. Missing
inputs fail the build; they do not enable another dependency source. Each
source tree must remain clean and must match the original source inventory.

The source and Go toolchain inventories contain regular-file hashes and
executable bits. Symbolic links and special files are rejected. The check
requires the pinned official SDK inventory before the first Go command. It
also compares toolchain and Git executable hashes before and after both builds.
The compiler must report the selected commit, clean source, and exact Go
version and target. A matching diagnostic record alone is insufficient: the
two executable hashes must also match.

Each command has a time limit and a 4 MiB output limit. Timeout or interruption
terminates its process group. The workflow has a 25-minute limit, and the build
step has an 18-minute limit. Go compilation uses two workers and a 1536 MiB Go
memory target. That target is not a kernel memory limit. Source and toolchain
scans have file, byte, directory-entry, and depth limits. Full builds run only
in CI. Small local tests cover input rejection and process cleanup without
building Go code.

## Saved records

A successful workflow saves an artifact named
`compiler-linux-amd64-<full-source-commit>` with three files:

- `stego-linux-amd64`: the executable that passed both-build comparison.
- `build.json`: the source and tool identities, fixed settings, module
  checksums, version record, executable size, and executable SHA-256.
- `SHA256SUMS`: checksums for the executable and build record.

Check the expected repository, workflow, source commit, and successful run
before retrieving an artifact. In its extracted directory, `sha256sum --check
SHA256SUMS` checks the saved file bytes. These checksums cannot authenticate
themselves. An attacker who replaces both files can also replace the checksum
list. Do not treat this record as a signed release or an installation policy.

The separate [origin verification procedure](compiler-provenance.md) adds
main-branch signatures and a fixed consumer policy. Its result is separate from
the unsigned build comparison described here.

## Limits and next work

This check uses two isolated build directories on one runner with one Go
toolchain. It tests reproducibility across source paths and caches. It does
not establish an independent build, trusted runner operating-system identity,
an independent toolchain build, or resistance to a compromised builder. System libraries
used by the Git tool are not included in the toolchain inventory.

Authenticated artifact provenance, release verification, supported-platform
coverage, and complete application build inputs remain part of C3. The existing
compiler and registry pins in Hypershell are unchanged. A successful result
from this workflow must be inspected before any further completion claim.

## Verified build result

[Artifact run 35188962614](https://github.com/jsell-rh/stego/actions/runs/35188962614)
passed at `7b75de63973119c8d754b9fef860a7f0883dd9a4`. Both isolated builds
produced the same 31,706,776-byte executable. Its SHA-256 is
`d302afe52ae36c675178c7366da9569a5e5e1943d7bfef0f26261b1229e5f126`.
The build-record SHA-256 is
`77ba6bd855fdf37290744883e78092b8482e1fc108283f7d81c2c2271ccccd40`.

Independent verification of the downloaded artifact passed. All 1,181 source
files match a Git archive of that exact commit. All 21 module records match
its `go.sum`. Static inspection of the executable found 20 matching dependency
records and the expected source revision, clean state, target, disabled CGO,
and path-removal setting. This inspection did not execute the downloaded
compiler on the workstation.

The source inventory covers 9,390,237 bytes with digest
`d54e1a544afca2ea024c0bf3b0dc6437d3fc11d8bbe2df2d1ee692638b5b1bf2`.
The CI toolchain inventory covers 15,036 files and 232,512,886 bytes with digest
`8a104c8dbc63490ec8282e81bf2c637062fe7a92e8c176a17cd38ce7408e0d8e`.
The toolchain inventory is a recorded build input; it is not independent
proof of toolchain provenance.

Seven small control tests passed locally and in CI. They include changed,
untracked, and ignored source files; links and size limits; inherited settings;
module checksums; output limits; and termination of children after their parent
exits. The existing full compiler suite is a separate check. These results
verify this build procedure, not the remaining C3 requirements listed above.

[Full compiler run 35188962575](https://github.com/jsell-rh/stego/actions/runs/35188962575)
also passed all six jobs at the same source. The race suite passed; the command
package took 55.889 seconds and the compiler package took 93.496 seconds.
Both examples, Keycloak, PostgreSQL provisioning, and resource-state storage
checks passed. The verified build check is now on remote `main`. The compiler
and common registry pins in Hypershell remain at their qualified runtime revision.

The main-branch repeat,
[35189507534](https://github.com/jsell-rh/stego/actions/runs/35189507534), also
passed at `7b75de6`. GitHub records distinct hosted-runner IDs for the two
runs. Each run made two isolated builds. Independent inspection found identical
compiler bytes and identical build records across both runs. This adds a
repeat on another hosted runner; both runs still use the same build procedure,
Go toolchain, and hosting provider.

The main-branch full compiler repeat,
[35189507480](https://github.com/jsell-rh/stego/actions/runs/35189507480), passed
all six jobs at the same exact `7b75de6` source. This is a separate result from
the artifact comparison. It does not close the remaining C3 requirements.

## Official SDK byte comparison

An independent check compared the SDK inventory from artifact run
[35190587406](https://github.com/jsell-rh/stego/actions/runs/35190587406), at
`842728b`, with the [official Go release archive](https://go.dev/dl/).
The selected archive is `go1.26.8.linux-amd64.tar.gz`, with 66,897,291 bytes and
SHA-256 `d0f743b33e8d8945e6b1f432edd15785c70507121d6e2a723b21285eddf8b57b`.
The downloaded archive matched that published size and digest. The check read
the archive without extracting or executing its tools.

All 15,036 file paths and all 232,512,886 file bytes match the reported SDK.
The executable bits differ. The official archive marks 54 files executable;
the reported CI inventory matches the same files with every executable bit set.
Its digest is `8a104c8dbc63490ec8282e81bf2c637062fe7a92e8c176a17cd38ce7408e0d8e`.
The official inventory with its original executable bits has digest
`94168e19a28c7bdeaf3c281f88e3a3efab13f7d80e2694ae2dd4d71378da9289`.

This explains the inventory difference and establishes official SDK file bytes
for this recorded build. It does not establish which process changed the
permissions, an independent toolchain build, or an uncompromised runner.
That historical build did not enforce this official-archive comparison. The
current procedure below adds the check before SDK execution.

## SDK preparation before execution

The artifact job no longer selects Go from the runner's tool cache. It uses
`scripts/prepare-compiler-toolchain.py` to download the fixed official archive
over verified HTTPS. The archive URL, exact size, SHA-256, and extracted
inventory are pinned in `scripts/check-compiler-artifact.py`. Changing the SDK
requires a reviewed source change. No latest-version lookup occurs in CI.

Preparation captures a private copy and checks its size and SHA-256 before
archive parsing. Extraction permits regular files and directories only. It
rejects links, devices, sparse files, repeated paths, path traversal, and special
permission bits. File count, total size, individual file size, path length,
and path depth have limits. Extracted files retain the official executable
bits. The resulting inventory must equal all 15,036 pinned file records.
The earlier cache layout with all files executable is no longer accepted.

The downloader has connection, transfer, output, and process time limits. It
uses system TLS roots and does not inherit proxy, loader, credential, or curl
configuration. It does not follow redirects or select another source on failure.
Preparation never executes a program from the SDK. It writes `toolchain.json`
after the check. Failure removes the new incomplete directory; existing output
is never replaced. The build repeats the inventory check before `go version`
and after both builds. Its signed build record includes the official release
identity as well as the observed toolchain inventory.

For an offline SDK input, supply the archive explicitly:

```sh
python3 -B scripts/prepare-compiler-toolchain.py \
  --archive /inputs/go1.26.8.linux-amd64.tar.gz \
  --output /private/compiler-sdk
```

The output parent must exist. The output path must not exist. The same checksum
and inventory rules apply to downloaded and offline input. This supplies the
SDK only; it does not supply compiler source, modules, or application inputs.

Ten small SDK checks cover capture, extraction, fixed download policy, changed
inputs, and failure cleanup. An additional build check requires rejection of a
substitute SDK before any command executes. The real archive preparation and
two full compiler builds must also pass CI. These checks do not prove an
uncompromised runner, system Python, curl, TLS store, or operating system.

## Verified SDK preparation result

[Artifact run 35193583033](https://github.com/jsell-rh/stego/actions/runs/35193583033)
passed at `403a7eb9fdc8bfbba41abc92021eeb8e5490727a`. Preparation accepted the
official archive before any SDK program ran. Both isolated compiler builds
matched. Independent inspection verified all 1,187 source records, 21 module
records, 20 embedded dependency records, and the compiler build settings.

The observed SDK inventory equals the official inventory, including executable
bits: 15,036 files, 232,512,886 bytes, and SHA-256
`94168e19a28c7bdeaf3c281f88e3a3efab13f7d80e2694ae2dd4d71378da9289`.
The compiler has 31,706,776 bytes and SHA-256
`bc20ea0d930778a024d34538ae9e4002f4022e67e69b177eff44be3aa8a78238`.
The build-record SHA-256 is
`e2c32e263d1da8281cd438a95a54146f8266d82fb813199434dc7823ba4899d8`.
The downloaded compiler was not executed on the workstation.

[Full compiler run 35193583091](https://github.com/jsell-rh/stego/actions/runs/35193583091)
passed all six jobs at the same source. Its saved log SHA-256 is
`cd86e49c7f3d11088d208f49a31f928ac2b87984d9204caf09199b7e7699672c`.
The source is on remote `main`. Its
[main signing run 35194301580](https://github.com/jsell-rh/stego/actions/runs/35194301580)
passed. Independent verification at `2026-09-17T07:27:06Z` accepted both real
signatures and confirmed that the signed bytes match the checked branch build.
All four rejection cases passed in CI, followed by another successful check
of valid input. These checks do not close the remaining C3 requirements.
