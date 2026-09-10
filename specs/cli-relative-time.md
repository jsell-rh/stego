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

Hypershell commit `d4ea8724b44406e125bca46d06b3b0fe16ce6cb5` pins compiler
`5a2f13eec5e8a2ae633d02e9adf4d82f6dabe02e`. The application supplies one
`RelativeFlag` mapping on `expires_at`; the common runtime performs conversion.
Its service-account workflow passed in 26.83 seconds. It checked day and hour
durations, zero-contact rejection of invalid input, API lifetime limits, protected
credential output, Keycloak token issuance, denied elevation, restart, revocation,
and deletion. The selected expiry remained unchanged after restart.

All six generated CLI workflows passed in 87.367 seconds with PostgreSQL and
Keycloak required. They covered catalog, apply, OIDC login, Gateway, grants, and
service accounts. CLI and contract race tests and application static checks
passed. The compiler feature also passed
[CI](https://github.com/jsell-rh/stego/actions/runs/34493038544).

Only the generated CLI runtime and state changed among the 75 generated,
dependency, and state files. After the application commit,
`scripts/generate.sh --check` passed and all 75 hashes matched. Both feature
commits are on remote `main`. The full application and Kubernetes suites were
not repeated locally for this command-runtime change. The full CLI port and
enterprise goal remain open.
