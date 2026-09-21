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

Repeated string values can use the explicit JSON conversion below. Other
repeated values, maps, real oneof fields, and protobuf enums require an explicit
omission. Other stored JSON conversions and application-derived parameters are
not yet supported. A mapper does not provide REST conversion or public error mapping.
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
is recorded below; the remaining application scope stays open.

## Hypershell catalog acceptance

Hypershell main `ccf350b1` accepts runtime source `b145ee5b`. It uses generated
mappings for ManagedCluster, GatewayRelease, and GatewayNetwork. Hypershell
retains access checks, domain operations, and public error selection. Optional
field presence is preserved. Invalid stored text produces no partial response
or supplied error data.

Independent review checked 1,314 core cases, 120 focused response cases, five
restart cases, and 11 provider test roots. Regeneration matched 430 generated
and module files, 421 output hashes, and 41 input hashes. The patch was empty.
All seven application images passed content and signature review.

The complete live Gateway workflow passed all 11 required tests. Review checked
1,707 source files, 431 generation hashes, exact compiler bytes, REST and gRPC
access, events, restart, telemetry, four browser images, and cleanup. All seven
evidence readers completed without error. Both test fixtures were absent, the
shared Lease was free, and all 32 standing resources were unchanged.

See the [consumer evidence](catalog-mapping-consumer-evidence.json). Exact main
checks remain pending. Normal cleanup with 100 accounts took an observed
32.70 seconds; the 30-second target remains unmet. This result does not complete
Gateway or grant mapping, REST conversion, or production capacity verification.

## Bounded JSON string lists

The next compiler candidate adds `json_strings` for a JSON model field and a
repeated protobuf string field. It is not yet released or accepted by Hypershell.

```yaml
- target: tags
  source: tags
  conversion: json_strings
  max_bytes: 4096
  max_items: 32
  max_item_bytes: 128
```

All three limits are required positive integers. `max_bytes` limits the encoded
JSON before decoding. `max_items` limits the list length. `max_item_bytes` limits
each decoded UTF-8 string and cannot exceed `max_bytes`. The compiler permits at
most 16 MiB of combined JSON input and 65,536 combined items per mapping. These
are conversion bounds; they do not limit the number of application resources.

An absent or zero-length source and JSON `null` produce a nil list. JSON `[]`
produces an empty list. Order, duplicate strings, empty strings, valid Unicode,
and valid escapes are preserved. Each output owns its list and string data.

The converter rejects other JSON shapes, null list members, malformed input,
trailing values, invalid UTF-8, and unpaired UTF-16 surrogate escapes. It returns
a nil response and the same fixed conversion error used by scalar mappings.
The converter does not validate DNS names, select observations, or grant access.
Applications retain those rules. CI must verify the generated implementation
before this candidate can be released.
