# Runtime configuration

This component generates typed environment settings. Applications declare field
names, environment names, types, limits, and defaults. STEGO supplies parsing,
validation, private errors, and complete-result semantics. The component uses
only the Go standard library.

Add `runtime-configuration` to the application archetype. For example:

```yaml
overrides:
  runtime-configuration:
    groups:
      - name: Worker
        fields:
          - name: Address
            env: WIDGET_ADDRESS
            type: string
            min_length: 1
            max_length: 256
          - name: Resync
            env: WIDGET_RESYNC
            type: duration
            min: 1s
            max: 1h
            default: 30s
          - name: WatchLimit
            env: WIDGET_WATCH_LIMIT
            type: integer
            min: "0"
            max: "1000"
            default: "0"
          - name: Enabled
            env: WIDGET_ENABLED
            type: boolean
            default: "false"
```

`LoadWorker()` returns `(Worker, error)`. Call it before provider setup. Pass the
typed fields to provider constructors. `ReadWorker(lookup)` accepts an explicit
lookup function for tests or an operator-supplied snapshot. The lookup must be
trusted and return promptly. A group reads each declared field at most once. It
does not enumerate the process environment or read another group's settings.

A missing variable uses its declared default. With no default, it is an error.
An explicitly empty variable is a value; it does not select a default. A string
can permit empty values with `min_length: 0`. All defaults must be strings in the
declaration and must satisfy the same rules as runtime input.

String limits count UTF-8 bytes, with a maximum of 4,096 bytes. Invalid UTF-8 and
control characters are rejected. Values are not trimmed or expanded. Integers
use signed 64-bit decimal form, with no leading zeros or plus sign. Durations use
Go duration syntax and cannot exceed 64 input bytes. Integer and duration fields
require explicit inclusive `min` and `max` values. Boolean values must be exactly
`true` or `false`. No type conversion depends on an ambient locale.

Invalid declarations fail before output. A component can declare at most 32
groups, 64 fields per group, and 256 fields in total. Group and field names are
limited to 64 bytes. The encoded declaration cannot exceed 128 KiB. Names are
exported ASCII Go identifiers. Environment names use uppercase ASCII letters,
digits, and underscores, with a letter first, and cannot exceed 128 bytes.
Duplicate fields, duplicate
environment names within a group, and generated symbol collisions are rejected.
Different groups can use the same environment name.

A failed read returns the zero settings value and an error that identifies only
the declared group, field, and fixed reason. It retains no input value or parser
error. `errors.Is(err, ErrConfiguration)` identifies configuration errors.
`errors.As` can obtain `*Error` and its `Group`, `Field`, and `Reason` methods.
Reasons are `missing`, `invalid`, or `lookup` for a nil lookup callback.

Formatting a complete settings value redacts its contents. Implicit JSON export
fails. Field access is explicit and returns the actual value; applications must
not log private fields. Use Secret file references instead of embedding secret
values as declaration defaults. Redaction does not prevent explicit reflection
or copying fields to another object.

Provider constructors still validate endpoint, certificate, credential, and
domain contracts. This component does not read Secret files, open connections,
grant access, or infer dependencies. Groups are independent. A process-wide
environment change during a read is not an atomic snapshot. Select immutable
configuration before startup when a shared snapshot is required.
