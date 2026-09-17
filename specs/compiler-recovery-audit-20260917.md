# Compiler generation recovery review

This review covers the recovery part of C3 at compiler revision
`db75a77da272fe05bbf6fdc3a3d1e0fede298f70`. It does not close C3.
The current compiler, build identity code, and CI workflow match that revision.

[CI run 35168719235](https://github.com/jsell-rh/stego/actions/runs/35168719235)
passed all six jobs. The compiler job used
`go test -race -count=1 -mod=readonly ./...` on Linux. The compiler package
passed in 112.601 seconds, the command package in 73.880 seconds, and the
registry package in 5.516 seconds. Both example jobs passed repeated generation
and stable state checks. The saved CI log has SHA-256
`085dc4c181d2f03c7cc8846c6380a0a8f45635750587c7d258783fee2e446b73`.

## Inspected recovery behavior

| Requirement | Implementation and test evidence |
| --- | --- |
| Recover interrupted output changes | `transaction.go` saves a versioned journal before output writes, validates the full journal, writes state last, and checks final hashes before journal removal. `TestRecoverCompletesEveryInterruptedPhase` injects failures after preparation, four operations, and before completion. Recovery runs twice and must leave no drift or orphan file. |
| Recover after process exit | `TestRecoveryAfterProcessExit` starts a child process and exits it at three journal phases. The parent completes recovery and checks output and state. |
| Preserve conflicting edits | `TestRecoveryConflictDoesNotOverwriteFiles` checks a changed output file. `TestRecoveryPreservesNewSourceEdits` checks a source edit made after journal preparation. Recovery completes the saved output; a later plan uses the new source. |
| Reject damaged recovery records | `TestRecoveryRejectsCorruptRecordsBeforeWriting` checks malformed paths, snapshots, contents, and state. The complete record is validated before writes. |
| Permit one writer | `TestConcurrentApplyHasOneWriter` requires one successful apply and no mixed output. `TestProjectLockIsReleasedWhenProcessDies` checks exclusion across processes and lock release after process death. |
| Recover dependency changes | `TestDependencyRecoveryCompletesBothModuleFiles` interrupts the module update and recovers it through the same journal. `TestDependencyFailuresPreserveProjectFiles` checks failure before commit. Module files retain application ownership. |
| Reject changed inputs | Snapshot and registry tests reject project, registry, and plan changes after capture. Dependency tests retain the project lock and reject changed local replacements before commit. |

These tests cover controlled failures and process exits. They do not simulate
power loss, damaged storage, or network filesystem locking. This CI result also
does not prove the Windows or other operating-system implementations.

## Remaining C3 work

The [build record](build-identity.md) contains selected diagnostic metadata.
It is not a compiler artifact digest or signature. Two modified compiler builds
can report the same metadata. Compiler artifact verification and controlled
builds remain open.

The [project input manifest](project-input-manifests.md) identifies captured
generation inputs. It does not identify every external tool, inherited build
environment setting, dependency source tree, or application source file.
Dependency commands disable workspace and alternate-module flags but inherit
other environment settings. These checks do not establish a fully controlled
application build.

The transaction and input manifest each have an explicit version. The reviewed
`db75a77` state record has strict field decoding but no separate version field.
The [state format change](state-format.md) adds an outer version and defines
legacy upgrade and downgrade rules. Its focused checks passed; full compiler
and example CI remains required. Compiler artifact trust and complete build
input identity remain part of C3.

Do not replace these open requirements with repeated output hashes. Repeated
generation and safe journal recovery are necessary evidence, but they do not
establish compiler artifact trust or complete build input identity.
