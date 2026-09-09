The `http-application` component connects a domain HTTP factory to the generated
server. The factory remains outside generated output. The `factory_package`
setting is its path within the application module. Its exported `New` function
accepts the public storage/transaction interface, a JWT verifier, and the shared
`database/sql` connection. It returns an HTTP handler and an error.

The component uses the PostgreSQL adapter and `jwt-auth` in `verifier` mode.
The generated runtime owns the database, server limits, signal handling, request
draining, and background tasks. Application code owns routes, request/response
mapping, domain operations, and error mapping. Domain-specific SQL queries can
use the supplied connection. They must use parameters and bounded contexts.

The generated `transport.Endpoint` function binds typed decoding and domain
operations. It authenticates before decoding, passes the verified request
context to domain code, and limits the operation to ten seconds. Cancellation
closes a blocked request body. Domain errors go to the application's error
mapper. Responses disable caching. Encoded responses above 8 MiB are rejected.
HTTP 204 and 205 endpoints require the `transport.NoContent` response type. They
return no body or content type. A mismatched response type fails endpoint
construction. Independent Record tests check both status codes over a real
HTTP connection, including denied and unauthenticated requests.

`transport.JSONBody` accepts one JSON object of at most 64 KiB. It rejects
unknown or duplicate members, case aliases, trailing values, invalid UTF-8,
unpaired UTF-16 surrogates, and nesting above 32 levels. Struct fields need
explicit unique JSON tags. The Unicode escape check is also used by the JWT
verifier, so invalid identity strings cannot become replacement characters.

A separate Record application tests typed decoding, nested fields, verified
identity, malformed requests, failure mapping, and cancellation. Its assembled
main program compiles. The Hypershell tests execute the generated process with
PostgreSQL, signed tokens, and a mutual-TLS Kafka protocol fixture.
The cancellation checks include a stalled body on a real socket. The handler
sets a read deadline and waits for cancellation cleanup before it returns.
Assembly rejects a missing verifier dependency before output is written.

The PostgreSQL `migrations: external` option removes automatic schema changes
from startup. The existing `startup` option remains the default for earlier
services. The Hypershell variant uses external migrations and applies schema
changes during test setup. A production migration runner remains required.

The public `ListOptions.CountOnly` option returns the filtered total and an
empty typed result. It does not fetch resource rows. This supports APIs where
an explicit zero page size requests a count. It leaves the existing zero-value
storage options unchanged.

An application handler can implement `Run(context.Context) error` and `Close()`.
The generated runtime then supervises its task and closes its resources during
shutdown or later startup failure. Both methods are required together. A plain
HTTP handler remains valid and has no application task beyond waiting for
shutdown. The wrapper calls application cleanup at most once.

`ReplyEndpoint` supports operations that return different successful statuses.
Its typed `Reply[T]` requires a non-nil value for a response body. HTTP 204 and
205 require a nil value. Invalid status and body combinations fail before a
success response is written. Authentication, decoding, deadlines, and response
limits are the same as `Endpoint`. This supports a durable operation that returns
202 while work is pending and 200 or 204 when the work is complete.

`Endpoint` and `ReplyEndpoint` accept one optional `Prepare` callback after the
error handler. The callback runs once after token verification and successful
input decoding, before the domain call. It uses the request deadline and must
honor cancellation. Its error goes to the application error handler and stops
the operation. It is not converted to a token error. A nil callback or more than
one callback is a configuration error. A callback that commits work must define
its own transaction; a later domain error does not undo that commit.
