A constructor dependency must identify one producer across the assembled
components. When two components derive the same value name, a constructor
argument or declared dependency cannot select either one by position. STEGO
rejects the ambiguity and names the consumer, dependency, and producers.
Producer names in the error are sorted for stable diagnostics.

The Gateway tracing workflow exposed this requirement. Event delivery and
tracing both had `NewRuntime` constructors. A gRPC constructor that accepted
`runtime` could select event delivery when it needed tracing. If both values
had the same Go type, a build would not detect the wrong instance. Tracing now
uses `NewTracingRuntime` for assembly. The compiler also rejects this failure
class for other components.

Checks apply to explicit dependency metadata and value references in constructor
arguments, including constructors with no current consumer. The observer uses
the same Go syntax rules as constructor rewriting. Strings, selector member
names, explicit struct field keys, and imported package qualifiers are not value
references. Map keys and values can be dependencies. Closures or named literal
keys that require more scope or type information fail with a named-helper error.

Equal constructor names in separate components remain valid when routes or
middleware identify their own constructor by component or index. Cross-component
constructor dependencies need distinct producer names. This change does not
introduce a component-qualified dependency syntax or complete the typed wiring
work. Legacy inferred-dependency consumption and ordering need a separate audit.

Tests cover explicit and inferred ambiguity, producer order, unused consumers,
syntax distinctions, and same-name route constructors. Reconciliation must
return no plan, preserve an existing output file, and write no state when the
wiring is ambiguous. A valid application continues to use one tracing runtime
for HTTP and gRPC, separately from its event runtime.

The regression fails against compiler `e68f200` because it accepts ambiguous
wiring. After the fix, the full compiler race suite passed with PostgreSQL
required on port 32909. Static checks passed. An earlier full run found one
legacy test that required first-producer selection. Its valid unused-constructor
case remains covered, and the ambiguous dependency now has rejection tests.
