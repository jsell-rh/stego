# Stream startup contracts

The generated gRPC client now supplies `RequireStreamHeaders`. A controller can
require exact capability and scope headers before it accepts events. Each
`StreamHeader` requires one value. Missing headers, duplicate values, and wrong
values return `ErrStreamContract`. Additional, unrelated headers are permitted.

STEGO owns the stream-start procedure. Hypershell selects the database deletion
capability, the retained replay scope, and the controller error classification.
These domain names do not occur in the compiler implementation. The new helper
removes the header and early-error procedure from the database controller.

The helper reads the initial headers once per call. If the header is absent,
it receives the terminal RPC result. An RPC error is returned unchanged,
including its status code and details. This preserves the retry decision after
an error before headers. A successful empty stream cannot confirm a capability.
The [nil-header regression](grpc-stream-headers.md) remains covered separately.

If headers are present, the helper does not receive a message. This rule also
applies to invalid or incomplete headers. A caller can check several requirements
in one call. The helper does not modify the headers or the requirements.

Requirements are limited to 32 entries. Each name is limited to 256 bytes and
uses lowercase ASCII letters, digits, `-`, `_`, or `.`. Each value contains
1–1,024 printable ASCII bytes. Duplicate requirement names are invalid. Invalid
requirements and nil streams fail before stream access. Contract errors contain
no remote metadata values. Returned RPC errors retain their original details;
controllers must continue to use bounded error summaries for logs.

The caller owns the stream context and must cancel it on error. The stream must
honor that context and have a setup deadline. The generated client and controller
runtime supply these bounds. The helper does not start a worker, retry, reconnect,
or grant access. Calls must not race with another header or receive operation on
the same stream. The test double that sends a value before headers is rejected;
that behavior is outside the gRPC stream contract.

The generated runtime tests use real TLS connections. They check early RPC
errors, clean empty streams, missing and duplicate capabilities, wrong versions,
and the first event after both successful and failed validation. Unit tests
check several required headers, input bounds, nil streams, stream read counts,
and preservation of error identity and details.
