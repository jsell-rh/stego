# Generated Go symbol bindings

The assembler now protects Go's predeclared names and its own startup
declarations when it assigns import aliases. A component or fill path that ends
in `error`, `nil`, `make`, `main`, or `run` receives a distinct alias. Package
references use that assigned alias. Language names in generated startup keep
their original meaning.

The regression test first produced a module that failed to compile. Imports hid
`error`, `nil`, and `make`, and imports named `main` and `run` conflicted with the
generated functions. The compiler had accepted and emitted those files. The
baseline test package failed in 0.373 seconds.

The alias allocator also checks each complete candidate name. If `api2` was
already assigned as a suffix for `api`, a later package whose base name is
`api2` receives another alias. A first use of a base name does not permit reuse
of an already assigned name. This rule also applies to constructor variables.

Fill construction now uses the alias map returned by import assembly. The
assembler no longer repeats the allocation sequence in a second function. Thus,
imports and calls to fill constructors use the same binding record.

A constructor's derived value name must not be predeclared in Go. For example,
`NewError` derives `error` and is rejected before output is returned. Use a
distinct constructor name such as `NewErrorHandler`. String expressions do not
carry enough binding information to distinguish every intended dependency from
a hidden language type, function, or literal. The compiler must not guess.
This validation also applies to constructors that have no consumer.

The generated-program test combines all predeclared names, names taken from
both startup templates, explicit suffix collisions, and fill imports. It enables
HTTP and background-task wiring, builds the module with race detection, runs its
startup, and checks that every component and fill constructor was called. It
also requires repeated assembly to produce identical files. Separate tests reject
predeclared constructor names without output and check repeated suffix allocation.

This is an improvement to the existing wiring contract. It does not replace
string constructor expressions with a typed intermediate representation. The
remaining work includes explicit value and type bindings, complete type checking,
and removal of other inferred constructor references. The enterprise goal is
not complete.

The generated-program checks passed with race detection in a 3.205-second
compiler package run. The complete compiler race suite and `go vet` also passed.
A local benchmark on 2026-09-10 used Go 1.26.8 on Linux amd64 and an Intel Core
Ultra 9 185H. Three 200 ms samples assembled 12 routed components and five fills.
Each assembly took 0.876–0.933 ms, allocated 490,861–491,088 bytes, and made 10,133
allocations. This measures assembly only; it excludes declaration parsing, file
writes, dependency resolution, and compilation of the generated program. There
is no comparison with the old implementation or production throughput claim.

```sh
go test -run '^$' -bench '^BenchmarkAssembleWithImportsAndFills$' \
  -benchmem -benchtime=200ms -count=3 ./internal/compiler
```
