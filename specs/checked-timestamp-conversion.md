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
mapping requirements. Hypershell adoption must also correct the unchecked
catalog timestamp path and prove failure without a partial response.

Compiler checks and a complete Hypershell workflow remain required. A passing
conversion test alone cannot qualify transport parity or an application change.
