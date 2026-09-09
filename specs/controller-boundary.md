The controller mechanism belongs in STEGO. The application supplies the rules
that determine the desired state and the permitted effects. A generated transport
client alone does not establish this boundary.

The first correction adds the `controller` component. Hypershell's Gateway
identity, Gateway workload, and managed-database controllers use it. Their
repeated queue, timer, reconnect, scan scheduling, and worker shutdown code was
removed. The component uses typed sources and actions without Hypershell names,
API paths, resource schemas, or provider rules.

| Responsibility | Owner | Current evidence or remaining work |
| --- | --- | --- |
| Watch setup, bounded queue, serial actions, deadlines, reconnect, and worker shutdown | STEGO | Shared runtime and generated race tests; three application controllers migrated |
| Failed-work and missed-deletion scheduling | STEGO mechanism, with an application source contract | Runtime repeats retained scans; storage and replay adapters still need generation |
| Cursor page loops, request limits, and complete-page validation | STEGO | Generated scanner; both Gateway controllers and database deletion replay use domain API mappings |
| Dirty-key scheduling, delayed retries, and duplicate suppression | STEGO | Generated keyed runtime; Pod count migration passed the local sandbox workload gate |
| Observation baseline, reset, cache bounds, and shutdown | STEGO | Kubernetes transport emits reset and replacement events; part of cache management remains in the application |
| Which Pods count as active and which Gateway owns them | Hypershell | Domain label, namespace, and phase rules |
| Bounded worker pools and fair recovery scans | STEGO | Generated sweep; service-account migration passed regression and real Keycloak workflows |
| When accounts expire and which actions can recover them | Hypershell | Domain state transitions, creator access, and credential rules |
| Leases, fencing, and coordination between controller processes | STEGO | Open; current controllers require one active process for each ownership scope |
| Placement, identity mapping, workload definitions, and cleanup order | Hypershell | Existing domain actions and provider tests |
| Provider transport, TLS, token files, request bounds, and cancellation | STEGO | Existing generated clients; domain providers call these clients |

The remaining application helpers are not presumed to be domain-specific.
Each later extraction must remove common application code and retain executable
workflow evidence. Do not create a broad framework whose only test is a mock
controller, or add a new copy of a mechanism in Hypershell.

The FIFO runtime preserves each item. It does not combine a live record with
a deletion record, supply distributed exclusion, or guarantee exactly-once writes.
Its source contract requires retained recovery data and cancellation. Its action
contract requires safe repeated execution and authoritative access checks. These
limits must remain explicit until the relevant mechanism and failure tests exist.

Further controller work must address shared storage adapters, durable claims,
and distributed fencing. Keep the existing sandbox-count and account-recovery
workflows as acceptance gates. Test missed events, restart, denied access,
incomplete scans, queue pressure, cancellation, and regeneration. Measure
throughput and memory before selecting production queue sizes or worker counts.
