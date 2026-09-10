# Constructor metadata validation

The assembler now checks every constructor index before it removes unused
constructors. An invalid reference cannot silently remove a declared middleware,
cleanup call, dependency record, or slot association. Validation returns an error
without generated files.

The checked fields are constructor resources, background tasks, error results,
collection associations, dependencies, cleanup calls, primary middleware, inner
middleware, and outer middleware. Negative indexes, indexes past the last
constructor, and references into an empty constructor list are invalid. The
checks also apply when the service has no HTTP routes or the constructor has no
consumer. A false error-result flag or an empty dependency list does not permit
an invalid index.

Primary middleware selection must be explicit. A wrapper expression requires a
constructor index, and at most one component can declare primary middleware.
Inner and outer middleware remain ordered lists and can contain several entries.
Their indexes and required wrapper expressions are checked individually.

The previous code skipped invalid middleware indexes during handler assembly.
Thus, a declared authentication layer could be absent from the generated handler.
It also ignored invalid collection, dependency, and cleanup indexes. The new
regression tests first returned generated files for those invalid records. They
also found accepted wrapper expressions without a constructor and multiple
primary middleware declarations. The baseline package failed in 0.016 seconds.
This is compiler-output evidence; that baseline did not send an HTTP request.

Each negative test first checks a valid metadata fixture with a valid Go version.
The test covers both services with routes and services without routes. It also
checks that every constructor-indexed map in the wiring schema has an index test.
An older background-task test was corrected: its missing Go version had caused
an earlier validation error, so it did not establish task-index validation.

Map indexes are checked in numeric order. Repeated invalid input must produce
the same diagnostic. Existing resource-kind, task-uniqueness, and required-wrapper
checks now share the same validation step.

This change validates references and middleware selection. It does not provide
complete Go type checking or a typed constructor dependency graph. Those
requirements remain open. A valid index alone does not prove that a component
implements the security behavior promised by its contract.

The full repository tests passed with the race detector. `go vet ./...` also
passed. The compiler package completed in 23.932 seconds. Three benchmark samples
used Go 1.26.8 on Linux amd64 with an Intel Core Ultra 9 185H. Validation of 256
constructors with all indexed maps, tasks, and inner middleware took
85,713–91,599 ns per operation, with 29,096–29,098 bytes and 18 allocations.
The existing assembly benchmark took 813,356–883,863 ns per operation, with
491,145–491,650 bytes and 10,133 allocations. These are local measurements;
they do not establish a production performance limit.

The Hypershell variant pins compiler commit
`6cd73171452ec92731bd897859824f72f88cb4a0` in application commit
`0ab80d5635a35067ef87b2098433ffc8c8af9f11`. Generation before and after the
application commit preserved all 74 generated and dependency file hashes.
Contract race tests passed in 1.542 seconds, and the application build passed.
`scripts/generate.sh --check` also passed after the commit. Both repositories
were pushed to `main`.

The full PostgreSQL, Keycloak, and Kubernetes workflows were not repeated for
this compiler pin change. Application source, generated files, and dependencies
are unchanged from the prior tested application. The earlier full workflow
results remain recorded in [database recovery](https://github.com/jsell-rh/hypershell-stego/blob/main/acceptance/database-recovery.md).
