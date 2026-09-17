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
compares toolchain and Git executable hashes before and after both builds.
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

## Limits and next work

This check uses two isolated build directories on one runner with one Go
toolchain. It tests reproducibility across source paths and caches. It does
not establish an independent build, trusted runner operating-system identity,
toolchain provenance, or resistance to a compromised builder. System libraries
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
