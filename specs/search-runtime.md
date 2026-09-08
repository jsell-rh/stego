The generated search engine accepts declared fields and common metadata names.
It uses the pinned TSL grammar with a separate SQL conversion step. Each
identifier must resolve to a declared column. Identifiers are quoted, values
are bound as parameters, and Boolean expressions keep explicit parentheses.
This prevents an `OR` expression from changing an independent access filter.

Limits apply before parsing: 4 KiB of valid UTF-8, 512 tokens, and 32 levels of
parentheses. SQL conversion also has a depth limit. Each `IN` list has at most
128 values. Numeric source text is preserved, including integers above the
exact range of float64. Each parser has private DFA caches because the pinned
ANTLR grammar shares mutable caches by default. Its parser stage also uses a
lock because lookahead sets are stored on shared grammar states. SQL conversion
and field validation use request-local data.

The SQL conversion checks field and operand types. PostgreSQL data conversion
errors caused by search values become `ErrSearch`. Other database failures keep
their original error class. A store without a search component rejects search
input. It does not silently return an unfiltered result.

The HTTP transport supplies an ordering parser with a caller-supplied field
map. It accepts at most eight terms and 512 bytes. Unknown fields, duplicate
columns, extra words, and invalid directions fail. The PostgreSQL adapter also
checks ordering before a count-only response.

Tests execute the generated search code with the race detector. Separate
PostgreSQL Record/Membership tests check access filters, exact large integers,
invalid fields and values, and count-only ordering. Hypershell process tests
check search, ordering, counts, and denied data through REST.

Related-resource paths, custom field resolvers, and regular-expression operators
are not implemented by this search engine. Broader query compatibility and
production capacity remain open. The existing registry resolver slot declaration
does not establish an implemented resolver contract.

A local parser benchmark used Go 1.26.8 on an Intel Core Ultra 9 185H. A query
with four conditions took 0.554 ms per operation across 1,000 iterations, with
561,933 bytes and 6,786 allocations per operation. The parallel benchmark took
0.353 ms per operation as a throughput measure. These results include parsing,
field checks, and SQL conversion. They exclude database and network time. Run
`STEGO_BENCH_SEARCH=1 go test -v ./internal/generator/tslsearch -run TestGeneratedSearchRuntime -count=1`.
The parser allocation cost remains a performance limit to assess with larger
application workloads.
