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

Nullable fields need three source states. The current model provider contract
cannot supply these states, so this release rejects such conversions. Explicit
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
