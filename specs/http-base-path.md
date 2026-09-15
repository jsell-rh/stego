# Literal HTTP base paths

`base_path` is a literal service prefix. Omit it, or use an empty string, to
serve collections from the root. A nonempty prefix starts with `/` and has
nonempty ASCII segments. Each segment can contain letters, digits, `-`, `.`,
`_`, and `~`. A segment cannot be `.` or `..`. Do not supply a trailing slash,
encoded characters, query, fragment, or path parameter.

The compiler does not clean or decode this value. Such a change could make the
generated route differ from the declared path or response link. Declare scoped
path parameters on collections, outside the literal service prefix.

Before this check, `/api/{id}` passed validation and produced a read pattern
such as `GET /api/{id}/users/{id}`. The regression registered the generated
patterns with Go's actual HTTP multiplexer and observed a panic for the repeated
wildcard name. Quotes and line breaks also reached route expressions. Go treats
braces as route syntax and rejects invalid patterns during registration; see
[the ServeMux contract](https://pkg.go.dev/net/http#ServeMux).

The common semantic gate and the REST generator now use the same prefix check.
`validate`, `plan`, and `apply` reject invalid prefixes before rendering output.
Direct REST generator calls also reject them, including calls with no
collections. `rest-api` 3.0.3 records this corrected input contract. Generated
runtime code for supported prefixes is unchanged.

Small checks cover 20 invalid prefixes, six valid prefixes through generated
route registration and in-memory HTTP requests, the shared gate before any
generator runs, and file preservation through all three CLI commands. The first
regression failed on the old generator. The corrected checks pass. Full compiler
CI remains required.

This result covers service `base_path`. The review also found that collection
`path_prefix` and cross-collection pattern conflicts need further validation.
Those checks remain open; this result does not establish their safety.
