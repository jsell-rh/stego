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

## Collection paths and route conflicts

`rest-api` 3.0.4 also checks resolved collection paths. Literal segments follow
the base-path rules. A parameter occupies a complete segment and uses an ASCII
identifier. Parameter names must be unique in the resolved path. The name `id`
is reserved for generated item routes. Catch-all parameters are not supported.
Full scoped paths and leaf-segment overrides retain their declared names.

The generator registers the proposed method and path patterns with Go's HTTP
multiplexer before it renders files. This rejects invalid patterns and route
conflicts. One route definition supplies validation and generated registration.
The compiler rejects legacy `httpmuxgo121` mode because that mode would bypass
the pattern check. The check does not open a network connection.

Public discovery paths must not hide a collection route. Collection and item
paths also cannot differ only in parameter names, even when their methods differ.
This follows the [OpenAPI path contract](https://spec.openapis.org/oas/v3.1.0.html#paths-object).
The existing case-insensitive collision rule remains in force.

Regressions first reproduced invalid registration, wildcard conflicts, hidden
collection routes, and equivalent OpenAPI paths across methods. Small checks now
pass for 22 invalid collection paths, both conflict forms, three discovery
collisions, scoped route requests, parameter names in handlers and OpenAPI,
legacy mux mode, and file preservation through all three CLI commands.

The first full compiler run for 3.0.3 failed because the registry test still
expected 3.0.2. The test now expects 3.0.4 and passes its focused check. Full CI
for the complete route change remains required. This check covers REST
collections and their discovery routes. It does not establish that all routes
from other components can be composed without conflicts.
