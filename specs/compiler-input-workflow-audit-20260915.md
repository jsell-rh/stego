# Compiler input and project workflow audit

This review checks C1 and C2 against compiler revision
`b0bd9a49722b0c2c8a0a1142a59e91c46bc2d6b6`.
[CI run 35012238080](https://github.com/jsell-rh/stego/actions/runs/35012238080)
passed all four jobs. Its compiler job runs `go test -race -count=1
-mod=readonly ./...`, with PostgreSQL and Node checks required.
The source under `cmd`, `internal`, `registry`, and `.github` has no change
between that revision and the reviewed documentation revision `bd827c6`.
No new test run was required for this documentation review.

The downloaded CI log confirms these package results:

| Package | Result |
| --- | --- |
| `cmd/stego` | Passed in 39.137 seconds |
| `internal/compiler` | Passed in 76.023 seconds |
| `internal/parser` | Passed in 1.239 seconds |

C1 is verified for the current compiler input contract:

| Required behavior | Inspected evidence |
| --- | --- |
| Reject unknown fields and duplicate keys | `internal/parser/strict_test.go`: `TestStrictServiceInput` and `TestStrictOtherDeclarations` cover service, nested constraints, collections, slots, dynamic configuration, and registry declarations. |
| Reject invalid constraints before output changes | `cmd/stego/validation_test.go`: `TestCommandsRejectInvalidConstraintsBeforeWriting` invokes validate, plan, and apply and compares the source, output, state, and module files. |
| Use one semantic validation stage | `Validate` and `Reconcile` call `validateSource`. `internal/compiler/validation_gate_test.go` compares their failures and supplies generators that fail if invoked after invalid input. |
| Reject missing capabilities | `TestMissingGeneratorStopsAllGeneration` covers missing, nil, and typed-nil generators. `TestInvalidComponentOverridesGateCompilation` covers unknown and incorrectly typed component settings. |
| Reject unsafe paths before writes | `internal/compiler/path_test.go` covers generated, deleted, and state paths. CLI tests cover invalid package namespaces and application factories. |

C2 is verified for the fresh REST project and fill workflow required by the
original assessment. `cmd/stego/scaffold_test.go:TestFillWorkflowBuild` creates
a fresh directory and performs init, fill creation, apply, dependency resolution,
Go tests, and a service build. It then repeats apply and dependency resolution,
runs tests and module tidy, checks unchanged module and checksum bytes, and
requires an empty plan and no drift. It also verifies that apply preserves the
human-owned fill. The generated unfinished policy returns `ErrNotImplemented`;
the test does not treat an unfinished business policy as a working application.

The example jobs separately passed repeated generation, stable state bytes,
drift, vulnerability scans, fill tests, and service builds for both examples.

These results close the two stated requirements at the recorded revision.
They do not prove every possible input, sustained fuzzing, all deployment
platforms, or the full Hypershell application. C3 through C7 and H1 through H3
remain subject to their own complete evidence. Future compiler changes must
retain these checks. The raw CI log is in persistent local storage at
`~/.local/state/stego/runs/compiler-35012238080.log`.
