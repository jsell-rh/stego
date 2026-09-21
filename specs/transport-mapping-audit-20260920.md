# Transport mapping audit

This source review covers Hypershell candidate
`e0f3d7bb9ae93666e9a1381137a57df57a0416f8`. It does not establish runtime parity
or complete the transport extraction. The candidate remains under qualification.

The handwritten adapters contain 2,202 lines, excluding tests: 1,108 lines in
eight `internal/httpapi` files and 1,094 lines in ten `internal/grpcapi` files.
Line count alone does not identify common code. These files contain both field
conversion and application access rules.

## Existing separation

HTTP endpoint wrappers already call STEGO `transport.Endpoint` and
`transport.ReplyEndpoint`. Field selection and order parsing also use generated
transport helpers. The gRPC adapter uses STEGO request preparation and resource
version parsing. Preserve these common mechanisms. A second request framework
would duplicate them.

## Contract boundaries

| Source | Observed behavior | Requirement for further extraction |
| --- | --- | --- |
| `httpapi/http.go`: `present`; `grpcapi/gateways.go`: `present` | Both select current Gateway observations and decode stored DNS names. REST also supplies `created_by`. | Keep observation selection and creator policy in Hypershell. Generate only declared field conversion after that selection. |
| `grpcapi/gateways.go`: `present`; `grpcapi/grants.go`: `presentGrant` | Both check converted protobuf timestamps before returning a response. | Use a common checked conversion. On failure, return no partial response and no supplied value in the error. |
| `grpcapi/catalog.go`: `catalogMetadata` | Catalog timestamps are converted without the checks used by Gateway and grant responses. | Add an invalid stored-time case before conversion extraction. This source difference is observed; its externally visible effect has not been tested. |
| `httpapi/http.go`: `writeError`; `httpapi/service_accounts.go`: `accountError`; `grpcapi/gateways.go`: `mapError` | The adapters have three distinct public error contracts. HTTP cancellation maps to 503; gRPC preserves its context status. Account errors use a different response body from catalog and Gateway errors. | Keep status, public message, body shape, and ordered error selection explicit. Do not infer one contract from another. Never expose the underlying error text. |
| `grpcapi/gateways.go`: `UpdateGateway` | Optional controller address fields or a conditional version select `UpdateControlPlane`. Other writes select `Update`. | Preserve the operation choice and domain authorization. Generic field copying must not authorize a controller write. |
| `httpapi/http.go`: `parseEntityPage`; `grpcapi/catalog.go`: `catalogPage` | REST starts with size 100 and rejects sizes above 100. gRPC defaults to 20 for sizes outside 1 through 500. | Preserve the declared protocol behavior. A common helper must not silently equalize these values or rejection rules. |
| `grpcapi/catalog.go`: `watchCatalog`; `grpcapi/grants.go`: `WatchRoleBindings` | Watch delivery reads the current domain view and access state. Grant replay also repeats the access check before each send. | Keep access checks at delivery. A generated mapper must not cache an authorization result or expose an unfiltered storage row. |

The immutable source is available in the
[HTTP adapter](https://github.com/jsell-rh/hypershell-stego/tree/e0f3d7bb9ae93666e9a1381137a57df57a0416f8/internal/httpapi)
and [gRPC adapter](https://github.com/jsell-rh/hypershell-stego/tree/e0f3d7bb9ae93666e9a1381137a57df57a0416f8/internal/grpcapi).

## Next implementation boundary

Start with checked response conversion in a proved workflow. Keep a typed input
that Hypershell has already authorized and prepared. Generate direct field
assignments and explicit checked conversions. Keep custom domain transforms as
typed application functions. Do not use runtime reflection to infer field
names, pointer presence, authorization, or update ownership.

Before a mapping is accepted, its declaration must account for each public
output field. Reject duplicate targets, unknown fields, unsupported conversions,
and incompatible optional values before output. A newly added contract field
must require a deliberate mapping or an explicit omission. Prove the mechanism
with a second service that has no Gateway types.

Required application cases include valid and invalid stored timestamps, absent
and empty optional fields, numeric bounds, invalid stored JSON, error privacy,
and failure with no partial output. Retain Gateway creation, atomic owner grants,
filtered lists, denied controller writes, event delivery, and restart checks
through REST and gRPC. Verify regeneration and the rendered browser workflow.

Error mapping can follow once the ordered contract rules have explicit tests.
The common mechanism may select a declared result for a known error. Hypershell
must still declare which errors mean forbidden access, conflict, quota failure,
or temporary provider failure. These meanings are application policy.

## Checked timestamp adoption

The audit above records the earlier source. Hypershell main `52001546` now
uses STEGO checked timestamps for Gateway, grant, catalog, and cleanup summary
responses. Catalog unary and list handlers return no partial response on failure.
Watches stop before an invalid message and close the subscription. Prior valid
messages remain delivered. The complete live workflow passed. See the
[consumer evidence](checked-timestamp-consumer-evidence.json).

This accepts one common conversion. It does not implement the full declared
mapping contract above. Field coverage, optional values, numeric bounds, stored
JSON, public error selection, and typed application transforms still need their
own compiler rules and evidence.
