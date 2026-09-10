This component generates an HTTP client for one HTTPS origin. It verifies TLS,
sets request and connection limits, and rejects redirects. The application
supplies its CA file and endpoint. Each request has a five-second deadline.

`Stream` reads newline-delimited frames through a GET request. It uses the same
origin, TLS, header, and connection controls as `Do`. A stream has a five-minute
deadline, a 4 MiB frame limit, and a 64 MiB total limit. Requests and streams share
16 request slots. The callback receives frames in order. Frame data is valid
only until the callback returns. The callback must honor its context.

A stream can fail after it delivered frames. The application protocol must
retain a cursor if it needs to resume. The client does not retry requests.
`Close` cancels active requests and streams and prevents new requests.

The generated transport also supplies `FieldProjector` for list responses.
Construct it with a `FieldSchema` of public JSON names. A nil child is an atomic
value; a nonempty child declares fields in an object or in object array elements.
Construction copies the schema. The schema has at most eight levels and 1024
fields in total. A schema name has at most 128 ASCII bytes: a letter or underscore
first, then letters, digits, underscores, or hyphens.

`Parse` accepts comma-separated field paths. A final `*` selects all declared
children. Empty input selects all declared fields. There are at most 64 paths,
eight segments per path, and 4096 input bytes. Unknown paths, duplicate paths,
empty terms, and invalid wildcards return `ErrRequest`. Validation does not
depend on whether the result contains rows. Selecting an object selects its
declared children; overlapping parent and child paths produce the same result
regardless of order.

`FieldSelection.ProjectList` takes an already authorized and presented response
and the name of its item array. It preserves the envelope, item order, exact JSON
numbers, explicit nulls, and absent optional fields. It returns only selected
public fields in each item. A wildcard cannot expose an undeclared field.
The selection and projector can be shared between concurrent calls.

The encoded response limit is 8 MiB. Validation rejects duplicate JSON members,
invalid Unicode escapes, more than 32 JSON levels, more than 1024 object members,
more than 1000 array elements, and more than 100000 values in total. List items
must be objects. Invalid schemas, response shapes, and zero-value selections
return `ErrProjection`. Applications must map this contract error to a server
error, not a successful partial response. The source must bound the Go values
before encoding; these checks cannot limit arbitrary application callbacks.

Selection belongs after access checks and presentation. It does not select SQL
columns, authorize a resource, change counts, or recover omitted data. An atomic
schema field permits its complete JSON value; declare child fields when the
value needs further restrictions. The application retains responsibility for
its public field declarations and for private data in the list envelope.
