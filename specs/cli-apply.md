The generated CLI can apply resource files. The application declares each
`ApplyResource` with a kind, optional API version, collection path, and separate
create and patch fields. The runtime expects the common REST shape: resource
objects have `id` and `name`; lists have `items` and `total`. No application kind
or path is built into STEGO.

A document has this form:

```yaml
apiVersion: example/v1
kind: Record
metadata:
  name: example
spec:
  count: 2
```

`apiVersion` can be absent for existing files. A supplied version must match the
application definition. `metadata.id` selects an existing resource directly.
The server still assigns IDs for new resources. `metadata.description` is
accepted only when the application declares that request field. A field cannot
occur in both metadata and spec. Unknown kinds, fields, versions, and types
cause an error. The input field checks cover the CLI contract. The API remains
the authority for domain rules, references, and permissions.

Use `apply -f FILE`, `apply -f DIRECTORY`, or `apply -f -` for stdin. Files can
contain JSON or multiple YAML documents. A directory walk reads `.yaml`, `.yml`,
and `.json` files in stable path order, including child directories. It rejects
symbolic links and non-regular input files. Directory access uses an `os.Root`.
The parser accepts the JSON subset of YAML. It rejects aliases, anchors, merge
keys, custom tags, duplicate keys, invalid Unicode, NUL, and non-JSON numbers.
It preserves integer precision.

The limits are 64 KiB per regular file and encoded document, 1 MiB for the whole
input, 128 files and documents, 1,024 directory entries, 16 directory levels,
and 32 document levels. Individual fields retain the CLI type and size limits.
Apply has a five-minute deadline. Each HTTP request also has the common client
network deadline. Cancellation closes a blocked stdin read and joins its wait
worker before input loading returns.

The command has three stages:

1. Read every file and validate every document against the declared fields.
2. Resolve every target and validate the selected create or patch operation.
3. Apply the planned operations in document order.

No resource mutation starts until all documents pass the first two stages.
Name lookup uses a quoted, escaped search value and requests at most two rows.
Zero matches select creation. One exact match selects patch. More than one
match is an error. An explicit ID uses a direct GET. Two documents cannot select
the same resource. Lookup failure, malformed responses, missing required create
fields, and ambiguous matches stop the command before resource writes.

`--dry-run` performs local input validation only. It does not read credentials,
refresh a session, contact an API, or choose between creation and patch. Its
result is `validated`. It does not claim that references exist or that the caller
has permission. This contract avoids hidden writes during a supposed dry run.
The caller can request a new private output file for the validation results.

Use `-o json` for structured results. Default output has one status line per
resource and quotes names to prevent terminal control sequences. Results contain
only kind, name, ID, and status. They do not contain spec values or API error
bodies. `--output-file` uses the common private-file contract and cannot replace
an existing file, including an input file.

| Status | Meaning |
| --- | --- |
| `validated` | Local dry-run input checks passed. |
| `created` | The create response passed identity checks. |
| `configured` | The patch response passed identity checks. |
| `failed` | The API returned a client error. |
| `unknown` | The write result is not established. Retrieve state before retry. |
| `not_attempted` | This resource write was not attempted. |

A failed write stops later writes, returns partial results, and produces a
nonzero process exit status. Earlier writes remain. The runtime does not retry
mutations. A server error, transport error, or invalid success response produces
an unknown result. A failed output write also reports that the request can have
succeeded. These rules prevent a false success report and blind mutation retries.

Name lookup and creation are separate requests. This is not an atomic server
upsert. If the application allows duplicate names, concurrent creation can
produce multiple records. Store the returned ID for later exact selection.
Multi-resource apply is also not one database transaction. Each API operation
must preserve its own transaction and event contract. This command does not
claim Kubernetes field ownership or pruning semantics.

Kustomize input is not implemented. `-k` returns an error before any request.
A later implementation must have a separate rendering and path-boundary test.
Unsupported kinds must also fail before writes instead of being skipped.

The generated Record and Widget tests cover create, patch, exact IDs, ambiguous
names, search escaping, integer precision, dry runs without credentials, invalid
later documents, preflight failure, partial failure, unknown write results,
private error text, file protection, directory order, input limits, stdin, and
cancellation. The complete compiler race suite and static checks passed for this
implementation. Application acceptance remains a separate gate.
