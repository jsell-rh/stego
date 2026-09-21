# Checked protobuf timestamp conversion

The generated gRPC transport supplies `Timestamp(time.Time)` for a required
instant and `OptionalTimestamp(*time.Time)` for an optional instant. Both return
`(*timestamppb.Timestamp, error)`. The application selects its public status and
response shape. Conversion does not select or authorize a storage row.

Required values must represent an instant from the start of year 1 through the
last nanosecond of year 9999 in UTC. A source location can place a local calendar
date outside these years while its UTC instant remains valid. The conversion
checks the instant. It preserves nanoseconds and omits location and monotonic
clock data, as required by the protobuf timestamp contract.

A nil optional input returns nil with no error. A non-nil zero Go time stays
present because it is the first supported instant. Invalid values return nil
and the fixed `ErrTimestamp` error. No supplied value enters that error. Each
successful call returns independent protobuf data.

The same source is generated for `grpc-application` and `grpc-processes`.
Independent generated tests use both flat and nested output namespaces. They
check the first and last supported instants, one nanosecond outside each bound,
large and negative years, local-zone boundary crossings, binary and JSON
round trips, optional presence, independent output, and fixed errors.

This helper does not generate complete response mappings. Domain observation
selection, field ownership, HTTP and gRPC error contracts, and delivery-time
access checks remain application responsibilities. The
[transport mapping audit](transport-mapping-audit-20260920.md) defines the next
mapping requirements. Hypershell now uses the checked catalog timestamp path
and rejects invalid values without a partial response.

Compiler checks and a complete Hypershell workflow are required for adoption.
A conversion test alone cannot qualify transport parity or an application change.

The immutable compiler release passed all 34 generated cases, all 37 compiler
packages, and both generated examples. Signature verification and a fresh
installation matched all five verified files. See the
[release evidence](checked-timestamp-release-evidence.json).
Hypershell main `52001546` accepts this helper after 100 focused cases,
1,294 core cases, seven image checks, and the complete live workflow. All 11
required live tests passed in 547.86 seconds. Review checked 1,684 source files,
430 generation hashes, telemetry, four browser views, and complete cleanup.
Main push checks are recorded separately and are still pending. See the
[consumer evidence](checked-timestamp-consumer-evidence.json).

The largest observed cleanup with 100 accounts was 30.84 seconds. The 30-second
target remains open. Complete response mapping and the enterprise requirements
also remain open.
