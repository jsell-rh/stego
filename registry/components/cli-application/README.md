The CLI component generates a separate command entry point, a command runtime,
and the common HTTPS client. Run or build `<out>/<namespace>/cmd`. The application
factory returns `command.Application` with command names, paths, methods,
request fields, and expected status codes. It supplies no networking code.

Set `factory_package` to a module-relative Go package outside generated output.
The package must export `Commands() command.Application`. The compiler validates
that package path. The generated runtime checks the returned definitions before
it reads configuration or sends requests. Definitions with duplicate names,
ambiguous prefixes, invalid routes, or invalid fields are rejected. Command
names have one through four words. The limit is 128 commands and 64 fields per
command. Hypershell names and rules do not occur in this component.

Common commands are `login --url URL --token-file FILE [--ca-file FILE]` and
`logout`. Login stores absolute file references in a private JSON configuration;
it does not copy the token or claim to have verified it with the API. A command
reads the current token file before its request. Logout removes configuration
and retains the externally owned token file. The factory selects the environment
variable and application directory used to locate configuration.

New directories use mode 0700. Configuration and token files must exclude group
and other access. The configuration directory must exclude writes by other
users. Configuration reads reject leaf symlinks. Writes use an open directory
handle, a random exclusive file, file sync, atomic rename, and directory sync.
Failed writes do not replace configuration with partial JSON. Input files must
be regular and bounded; a FIFO cannot block credential or body reads.

HTTPS and certificate verification are required. A supplied CA file selects an
explicit trust pool; omission selects system roots. The shared HTTP client does
not follow redirects or use environment proxy settings. CLI requests have a
15-second deadline. Mutation requests have no application retry loop. After an
uncertain failure, callers must check resource state before they retry a write.

Request fields support string, string-list, integer, and boolean values. A
string-list flag takes a JSON array. `--body FILE` accepts a JSON object and
cannot be combined with field flags. Unknown fields, duplicate keys, invalid
Unicode, invalid types, and unknown or repeated flags are rejected. Input
limits are 256 arguments, 4,096 bytes per argument or string field, 65,536 total
argument bytes, and a 65,536-byte body file. Responses have the shared 4 MiB
limit. JSON validation also limits depth and node count. Output retains exact
JSON numbers. Error responses expose the HTTP status and omit response bodies.

Commands marked for confirmation require `--yes`. Interactive prompts,
stdin bodies, browser and device login, token refresh, output files for secrets,
and automatic SDK generation are not supplied by this version. Factories must
not register a command that returns credentials until protected output support
is available. Current file operations target Linux; other platforms need build
and file-semantics checks before support can be claimed.

Generated tests use a Record application. They check request types, validation,
private files, token rotation, redirects, malformed responses, no response-status
retry, exact number output, and definition limits. The existing HTTP tests cover
transport failure, request completion, cancellation, and bounded timeouts.
