# Go SDK

This component generates typed Go clients from captured OpenAPI 3.0 contracts.
Select `go-sdk` in an archetype or mixin. Set `document` to the root contract and
list its other files in `references`.

STEGO supplies a bounded HTTPS transport. Select `otel-tracing` to include shared
logs, metrics, and traces. The application supplies no client transport code.
Call `sdk.NewClient` with a base URL, bearer token, and optional CA file and
request timeout. Call `Close` to release the client.

See the [input and runtime contract](../../../specs/go-sdk.md) for limits,
unsupported features, response handling, and acceptance requirements.
