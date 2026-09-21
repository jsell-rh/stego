# Response mappings

Compiler `6d68417d` adds declared protobuf response mappings to `grpc-application`
and `grpc-processes`. It does not change an API contract or select public error
statuses. Application adoption and live workflow checks are still required.

The application must first authorize the request and select its domain view.
For example, Hypershell must select current observations before it calls a
mapper. STEGO does not select observations, grant access, or update resources.

## Declaration

The following component configuration maps a prepared storage value:

```yaml
response_mappings:
  - name: Shipment
    provider: postgres-adapter
    model: Shipment
    message: shipping.v1.Shipment
    fields:
      - target: metadata.id
        source: id
      - target: metadata.kind
        constant: Shipment
      - target: metadata.href
        source: id
        prefix: /shipments/
      - target: metadata.created_at
        source: created_time
      - target: name
        source: name
      - target: count
        source: count
        conversion: int32
      - target: internal_tags
        omit: true
```

`message` is a complete protobuf message name. Target paths use protobuf field
names. Source names refer to the selected model declaration. The compiler binds
the source package to the provider's output namespace. A different model
provider can implement the same compiler contract.

The generated `mapping` package contains a typed function. For this example,
its signature is `Shipment(value store.Shipment) (*pb.Shipment, error)`.
The actual import paths follow the component namespaces. Generation uses the
protobuf compiler's Go field names, including reserved-name changes.

## Checks and behavior

Each output field must have a source, a string constant, nested field mappings,
or `omit: true`. A new public field requires a new rule. Unknown sources,
duplicate targets, parent and child overlap, unknown options, and unsafe type
changes fail before file generation. An omitted field keeps its protobuf zero
value. A mapped nested message is present, even when all its fields are omitted.

Scalar types must match. Storage enum and reference fields use strings.
The explicit `int32` conversion accepts `int64` sources and checks both bounds.
Timestamp conversion checks the protobuf range. String conversion checks UTF-8.
Optional source values require a target with presence. Required values mapped
to optional targets remain present, including empty strings and zero numbers.
The output owns its scalar pointers, timestamps, and byte buffers.

On a runtime conversion error, the mapper returns a nil response and
`mapping.ErrConversion`. The error contains no supplied value. The application
selects the public status and message. The mapper performs no I/O and retains
no state between calls.

The declaration permits at most 128 mappings, 512 fields per mapping, and eight
nested message levels. String constants and prefixes have a 4,096-byte limit.
These are compiler input bounds, not Gateway or service account limits.

## Current limits

Repeated values, maps, real oneof fields, and protobuf enums require an explicit
omission. Stored JSON conversion and application-derived parameters are not yet
supported. A mapper does not provide REST conversion or public error mapping.
Do not use an omission to hide a required application response field.

Model providers must describe the Go types they emit. They may expose scalar
fields and promoted fields through a single Go selector. Pointer traversal and
Go expressions are not accepted. Private storage state and relationships need
not enter this contract. Each consumer receives a separate copy. The compiler
rejects declarations that change during generation. Tests compare the built-in
storage declaration with its emitted Go syntax. This contract does not replace
Go type checking of an arbitrary third-party generator.

The release tests use a Shipment service with no Gateway types. They check
field coverage, optional values, numeric and timestamp bounds, invalid UTF-8,
error privacy, owned output data, and protobuf serialization in two namespaces.
The tests run in CI. A missing or incomplete CI result is not acceptance.


The signed release passed all nine compiler test groups and 24 generated
mapping cases in two namespaces. All 37 compiler packages, both generated
examples, 34 timestamp cases, and database access checks also passed. An
independent installation matched all five authenticated release files. See the
[release evidence](response-mapping-release-evidence.json). Hypershell adoption
and the complete application workflow remain open.

## Hypershell candidate

Candidate `b145ee5b` uses generated mappings for ManagedCluster, GatewayRelease,
and GatewayNetwork. Hypershell retains access checks, domain operations, and
public error selection. The candidate preserves optional field presence and
rejects invalid stored text without a partial response or supplied error data.

Independent review checked all 120 focused response cases, all five restart
cases, and the 11 provider test roots. Regeneration matched 430 generated and
module files, 421 output hashes, and 41 input hashes. The patch was empty.
See the [candidate evidence](catalog-mapping-candidate-evidence.json). Full
application checks, signed image review, and the complete live workflow are
still required. The candidate has not been promoted to Hypershell main.
