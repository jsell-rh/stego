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

The full compiler race suite and `go vet ./...` passed. The generated gRPC
package completed in 29.288 seconds. The Hypershell controller race tests passed
in 1.144 seconds; contract race tests passed in 1.433 seconds. Application static
checks also passed.

Three 200 ms benchmark samples used Go 1.26.8, Linux amd64, and an Intel Core
Ultra 9 185H. The benchmark calls the generated helper with a stream double that
returns an existing metadata map. One short requirement took 34.14–34.85 ns per
call; two took 63.47–66.71 ns. The maximum input set, with 32 names of 256 bytes
and values of 1,024 bytes, took 23,080–23,349 ns. All cases allocated zero bytes.
This measures validation only. It excludes network work, TLS, and the generated
client's metadata copies. The benchmark is in the generated runtime fixture;
the measured copy changed only its import to the pinned Hypershell client.

Hypershell commit `6e530b71716af805c0284ee51a4c51cc6094b9bf` pins compiler
`da993db0b506ff2ab0084c6c2a817f084efa9a69`. The compiler feature passed
[CI](https://github.com/jsell-rh/stego/actions/runs/34491896881).
The application uses the helper for live database watch and retained replay.
It preserves the existing watch and scan error classifications.

The real database gate passed in 89.440 seconds. The complete Gateway gate
passed in 206.710 seconds, including deletion before startup and the live
database and identity workflow. The application records the detailed scope in
[stream startup](https://github.com/jsell-rh/hypershell-stego/blob/6e530b71716af805c0284ee51a4c51cc6094b9bf/acceptance/stream-startup.md).
These checks include provider work, access denial, event delivery, restart,
retained recovery, and cleanup of late resources on a former cluster.

Post-commit `scripts/generate.sh --check` passed. All 75 generated, dependency,
and state hashes matched the pre-commit result. The previous 73 generated and
dependency files are unchanged; generation added one helper and updated state.
Both feature commits are on remote `main`. Local verification used the changed
consumer tests and both real workload gates; the full PostgreSQL and Keycloak
acceptance suite was not repeated locally for this refactor.
