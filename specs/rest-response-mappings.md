# REST response mappings

Status: released in compiler `ca25ab06`. Application adoption and the complete
REST workflow are not yet accepted. The requirements in
`rest-response-mapping-plan.md` remain in effect.

The HTTP application component can generate a public `contract` package from
captured OpenAPI inputs. It also generates a `responses` package that converts
prepared model values. It does not generate an HTTP client in either package.
The Go SDK keeps its existing public interface and separate private wire types.

Example configuration:

```yaml
overrides:
  http-application:
    factory_package: internal/httpapi
    document: contracts/openapi.yaml
    references: [contracts/objects.yaml]
    response_mappings:
      - name: Parcel
        provider: postgres-adapter
        model: Parcel
        schema: Parcel
        fields:
          - {target: id, source: id, omit_empty: true}
          - {target: kind, constant: Parcel}
          - {target: href, source: id, prefix: /parcels/}
          - {target: created_at, source: created_time}
          - {target: name, source: name}
```

Each target is a JSON property name in the selected OpenAPI object. The compiler
uses the backend's actual Go type and field declarations. It requires one rule
for each property, including properties in `allOf` references. It rejects
duplicate properties across composition branches, Go name collisions, unknown
rules, and incomplete mappings before output writes. Inputs follow the same
captured-file limits as the Go SDK.

Each source is a field in the selected provider's checked Go model contract.
Generated functions take that model by value and return a response pointer and
an error. The application must check access and prepare observations first.
The generated function performs no I/O and does not resolve creator or grant
names. A failed conversion returns a nil response and the fixed `ErrConversion`.

This release supports strings, booleans, matching integer and floating-point
types, and timestamps. An `int64` source can use `conversion: int32`; the function
checks both bounds. Strings must contain valid UTF-8. Floating-point values must
be finite. Timestamps must support JSON encoding without loss of precision or
UTC offset seconds. The function retains valid timezone offsets and fractional
seconds. Pointer values are copied into response-owned storage.

A non-pointer source mapped to an optional response pointer remains present,
including zero, false, and empty string. An absent source pointer remains absent.
A present empty source pointer remains present. `omit_empty: true` applies only
to a non-pointer string source and an optional response pointer. It omits that
property when the source string is empty. A constant or prefix is a bounded
string, not a Go expression. An optional property can use `omit: true` only when
the generated JSON field supports omission.

The original release requires three source states for nullable fields. Its model
provider contract cannot supply these states, so that release rejects such
conversions. The candidate below adds an explicit policy for a pointer source. Explicit
omission is permitted for optional nullable fields. Array conversion, prepared
application input fields, and Gateway and grant adoption remain open. They are
required follow-up work, not evidence of completed REST extraction.

Callers must stop on a conversion error before they send a successful response.
For a list, convert all rows before passing the result to the generated HTTP
transport. The application retains pagination, list metadata, field projection,
and error policy. The transport encodes the complete response before it writes
the success status.

## Release evidence

The exact main source passed all 38 tested compiler packages, both generated
examples, focused OpenAPI and SDK checks, generated REST runtime checks, and
database access checks. The artifact signature was independently verified.
Negative checks rejected a different source, changed compiler bytes with updated
checksums, a changed build record, and an invalid signature bundle.

The immutable release tag points to the tested source. All four release assets
match the verified artifact. A separate installation verified the package and
matched all five local verification files. The compiler was not executed on the
workstation. See the [release evidence](rest-response-mapping-release-evidence.json).

Hypershell catalog sources are prepared at `6b35ea25`. Regeneration and complete
application acceptance remain required. This release does not close the
remaining enterprise requirements.

## Candidate: bounded JSON string lists

The next compiler candidate adds `conversion: json_strings` for a non-pointer
JSON model field and a non-null OpenAPI array of non-null strings. This candidate
is not released or accepted for Hypershell. It uses the same checked decoder as
the gRPC mapping generator. The gRPC decoder output remains byte-identical.

Each rule must set `max_bytes`, `max_items`, and `max_item_bytes`. The compiler
permits at most 16 MiB of source bytes and 65,536 items per mapping function,
including repeated conversions of one source. Each item limit must fit within
its source byte limit. A rule cannot add a prefix or a constant.

The decoder rejects invalid UTF-8, unpaired Unicode surrogates, wrong item types,
trailing input, and exceeded limits. It returns no partial list. Each mapped
list owns its storage. Empty source bytes, JSON null, and an empty JSON array
produce an empty response array. An optional target can use `omit_empty: true`
to omit all three empty forms. Required targets cannot use this omission rule.
Explicit nullable response arrays and nullable items remain unsupported.

Example field rule:

```yaml
- target: tags
  source: tags
  conversion: json_strings
  max_bytes: 65536
  max_items: 128
  max_item_bytes: 256
  omit_empty: true
```

The candidate adds compiler rejection checks, aggregate-bound checks, generated
REST runtime checks, and shared-decoder output checks. CI must prove these
checks before release. Gateway adoption still needs typed application inputs
for domain values such as the creator name. The scalar catalog workflow uses
its frozen compiler and source while this candidate is checked.

## Candidate: typed application inputs

A mapping can declare `inputs` for values that the application must resolve.
STEGO generates a `<MappingName>Input` structure and a second function argument.
Mappings without `inputs` keep their existing function signature and output.
The application still checks access, selects current observations, and resolves
names before it calls the mapper. Generated code does not perform lookups.

Each input has an exported Go field name and a fixed type: `string`, `bool`,
`int32`, `int64`, `float`, `double`, `timestamp`, or `jsonb`. Scalar fields can
set `optional: true` to use a pointer. JSON fields use a byte slice and cannot
request a pointer. Declarations cannot select an import, callback, or Go
expression. Each declaration must have a mapping. The compiler rejects input
names that conflict, unused inputs, unknown types, and ambiguous source rules.

Example additions to a response mapping:

```yaml
inputs:
  - {name: Creator, type: string}
fields:
  # Other response fields must also have mapping rules.
  - {target: created_by, input: Creator, omit_empty: true}
```

An `input` rule selects the declared input instead of a provider field. It uses
the same type, presence, numeric, timestamp, and encoding checks as a `source`
rule. It can use the bounded JSON string-list conversion. A rule cannot combine
`input` with `source`, `constant`, or `omit`. Each result owns its pointer and
list storage. A conversion error returns no response and no supplied value.

This compiler candidate has no application acceptance yet. Required checks
include compiler rejection before output, unchanged mappings without inputs,
deterministic input order, prepared values distinct from stored values, and
ownership after source mutation. Gateway adoption and the full live workflow
remain required.


## Candidate: string enums

A response field can use a string enum from the captured OpenAPI contract.
STEGO checks the declared values and the actual Go type from the backend.
A stored value or typed application input must match a declared value. The
check runs after an optional prefix. A constant must match at compile time.
An unknown value returns `ErrConversion` with no response object or supplied
value in the error. The application retains its access and grant policy.

Optional fields retain pointer presence and owned storage. An empty string is
valid only if the contract declares it, or if `omit_empty` explicitly omits the
optional field. Nullable enums remain unsupported for response conversion.

Each enum has at most 256 distinct values. Each value has at most 4096 UTF-8
bytes. All values together have at most 65536 bytes. Unsupported backend types,
duplicate values, and invalid encodings stop generation. These compiler bounds
do not set a limit on application records.

This candidate adds model binding, compiler rejection, and generated runtime
checks. CI results and application adoption are still required.


## Candidate: explicit nullable scalar responses

Component version 1.13 adds an explicit presence policy for nullable scalar
responses. A pointer source must declare `on_absent: emit_null` or
`on_absent: omit`. The first policy writes JSON null when the pointer is nil.
The second omits the property, and is permitted only for an optional field that
supports JSON omission. A required nullable field cannot use `omit`.

```yaml
fields:
  - {target: description, source: description, on_absent: emit_null}
  - {target: optional_note, input: Note, on_absent: omit}
```

A present pointer retains its value, including an empty string, false, or zero.
A scalar source without a pointer always supplies a value and cannot declare
`on_absent`. String constants also supply values. The existing checks for
encoding, integer bounds, finite numbers, and timestamps remain in effect.
Each response field owns its nullable storage and copied scalar value. Failed
conversion returns no response and a private error without supplied values.

This policy maps two source states to an explicit subset of the three JSON
states. It does not supply three independent states in the source. Nullable
objects, arrays, and enums remain unsupported. The compiler rejects these
conversions. Explicit omission of an optional unsupported field remains valid.
The application retains access rules, credentials, and connection policy.

The candidate includes compiler checks before output and generated runtime
checks for presence, ownership, scalar bounds, and invalid values. CI, release
verification, and consumer adoption are still required. The running Hypershell
workflow uses its existing compiler and source.

## Bounded JSON object response conversion

The compiler candidate adds `conversion: json_object` for a JSON model field
or prepared input and a free-form, non-null OpenAPI object property. This is a
common conversion. The application still selects domain values and checks access.

```yaml
- target: metadata
  source: metadata
  conversion: json_object
  max_bytes: 65536
  max_nodes: 4096
  max_depth: 16
  max_scalar_bytes: 8192
  on_empty: omit
```

All four limits and `on_empty` are required. `max_bytes` limits raw JSON before
decoding. `max_nodes` counts containers, scalar values, and object names.
`max_depth` counts container levels, with the root object at level one.
`max_scalar_bytes` limits decoded names and strings, and JSON number text.
The compiler permits at most 16 MiB, 65,536 nodes, and 32 levels. A response
mapping shares its 16 MiB and 65,536-item allocation budgets across all JSON
list and object fields. These are declared conversion bounds, not Gateway or
account capacity limits.

Empty source bytes are absent. `on_empty: omit` requires an optional response
pointer with `omitempty`. `on_empty: reject` returns the private conversion
error for absent bytes and is required for a required object field. A present
empty object remains `{}`. An empty array remains `[]` inside an object. Nested
JSON null, false, empty strings, and zero values remain present. A root JSON
null, array, or scalar is rejected. Whitespace alone is not an absent value.

Numbers use `json.Number`; decoding does not round them through a floating-point
value. Returned maps, lists, names, and scalar values own their storage.
Different response fields do not share mutable maps or lists. Invalid syntax,
invalid UTF-8, lone UTF-16 surrogates, duplicate decoded object names, trailing
values, and limit violations return `ErrConversion` with no partial response.
The error contains no supplied data.

The conversion requires a free-form object. Typed additional properties,
declared properties, object enums, composition, nullable objects, and property
count constraints are rejected. Their checks must not disappear because a Go
backend uses a map. Full nullable object states remain outside this candidate.

Generated runtime, compiler preflight, existing list compatibility, and hosted
compiler checks remain required before release. Hypershell role response
adoption and the complete application workflow remain separate requirements.
