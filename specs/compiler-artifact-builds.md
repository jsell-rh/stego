# Compiler artifact build check

The `Compiler artifact check` workflow builds the Linux amd64 compiler twice
from one exact Git commit. Each build uses a separate source tree, home,
temporary directory, module cache, and build cache. It compares the executable
bytes, module checksum records, and compiler version records before it saves
an artifact. The first CI result for this check is pending.

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
