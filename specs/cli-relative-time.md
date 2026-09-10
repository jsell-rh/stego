# Relative timestamp flags

The generated CLI can map a duration flag to an existing timestamp property.
Set `RelativeFlag` on a string `Field`. For example, a field with flag `end-at`,
key `end_at`, and relative flag `end-in` accepts `--end-in 30d` and sends one
absolute UTC RFC 3339 timestamp in `end_at`.

This is common command behavior. Applications select the field and flag names.
The server retains its lifetime and access policy. STEGO contains no service
account names, role names, or application lifetime limits.

The relative flag is exclusive with the field's ordinary flag and with `--body`.
Ordinary string flags and body fields retain their existing behavior. Relative
flags are allowed only on string fields in POST, PUT, and PATCH body commands.
Aliases cannot collide with another field flag, a path flag, or a reserved flag.
Help output includes the relative flag and its exclusion rule.

Durations must be positive. Go duration syntax supports values such as `24h` and
`1h30m`. An integer with suffix `d` means exactly 24 hours per day. Fractional days
and mixed day/hour syntax are not supported. Input is limited to 64 bytes.
Duration overflow and results outside RFC 3339 calendar years are rejected.
Conversion keeps nanosecond precision and uses one command timestamp for all
relative fields. A local clock error can affect the requested timestamp; the
server must validate it against its own clock.

Invalid or conflicting relative values fail before output reservation,
credential refresh, or HTTP requests. An API rejection removes an unused
reserved credential-output file through the existing output contract. The
helper does not alter server defaults when the timestamp field is absent.

Generated tests cover offset conversion, leap dates, nanoseconds, overflow,
calendar bounds, invalid definitions, conflicting flags, help output, unchanged
absolute values and body files, and real TLS requests. Rejected input must cause
no HTTP request and leave no output file. The full compiler race suite and
`go vet ./...` passed.

The Hypershell baseline first rejected `--expires-in` as an unknown argument in
the service-account workflow. That run failed in 24.582 seconds before the API
could enforce its lifetime policy. It does not show an API authorization defect.
