# Resource conditions

An entity can declare `conditions`, a map from controller owner to condition
names. Conditions require `versioned` and `generation_fields`. The declaration
permits at most eight owners and 32 condition names. Owner names use the same
syntax as observation owners. Condition names start with an uppercase ASCII
letter and contain at most 63 ASCII letters or digits. Each owner has at least
one unique name.

For example, a Widget can declare `conditions: {worker: [Ready]}`. This is a
common storage contract. STEGO has no Gateway or provider-specific condition.
The application chooses names, safe reasons, and the meaning of success.

## Stored evidence

The generated model has an internal `ConditionState` JSONB column. It is not a
domain field and ordinary generated writes do not set it. Each condition stores
`status`, `reason`, `message`, `observed_generation`, and `last_transition_time`.
Status is `True`, `False`, or `Unknown`. Reason is a code of at most 63 ASCII
letters or digits, starting with an uppercase letter. Message is at most 1,024
UTF-8 bytes and has no control characters. The entire stored value is limited
to 65,536 bytes.

`ConditionWriter.ObserveConditionsIfVersion` requires a generated store
transaction, the resource revision read before work, and every condition in
one declared owner group. It cannot replace another owner's group. It rejects
an absent, deleted, or changed resource. A rejected write also prevents commit
if its caller ignores the error. Authorization and event insertion belong in
that transaction.

PostgreSQL supplies the desired generation and transition time. The transition
time changes when status changes. A reason or message change with the same
status preserves that time. This time describes the stored status transition;
it is not the time at which the external event occurred. A caller must avoid
unnecessary writes when its complete current result is unchanged.

`Conditions` returns stored evidence and a derived `Current` flag. The flag
requires a live resource and an exact generation match. Missing observations
have `Unknown` status. `CurrentConditions` hides an old status, reason, message,
and transition time. It returns `Unknown` with reason `ObservationPending`.
It preserves the observed generation for diagnosis. Stored old evidence is not
removed. The read rejects malformed data, unknown owners and names, null
members, and generations beyond the resource generation.

A current generation does not prove controller liveness or bound observation
age. Repeated observation and controller diagnostics remain required. Conditions
are not a lease, a provider fence, or proof that related resources are current.
An application must define and check those dependencies before it claims success.

## Migration and input checks

Apply `000007_resource_conditions.sql` before new application code starts when
migrations run externally. Startup verifies the condition column and the exact
resource trigger. The trigger checks stored shape, declared owners and names,
status, reason, message bounds, generation, and time.

Migration validates retained condition values. It does not silently remove
observed owners or condition names. A changed condition contract changes the
resource trigger and invalidates previous generation observations. Migration
can scan retained rows; use the external migration path for planned deployment.
Application database roles must not have permission to alter these safeguards.

## Hypershell evidence

The application regression first failed after a real provider call: the private
recovery API had no durable identity failure condition. Hypershell now declares
`identity/ClientReady` and publishes it with the OIDC configuration and one event.
Its service holds the live Gateway row lock, checks the revision read before
provider work, and maps reason codes to fixed text. It does not accept provider
error text as a condition message. After an OIDC change in that transaction, it
records the condition at the resulting generation. No other writer can change
the locked input during that operation.

The condition covers the Gateway identity client. It does not certify user-grant
synchronization. That work needs separate dependency and condition rules. A
provider error is `Unknown`, because an unavailable provider does not prove that
the existing identity client is absent or broken.

The PostgreSQL and gRPC application test verifies failure persistence across API
restart, safe messages, stable transition time, generation invalidation, stale
write rejection, denied writers, recovery, unchanged-state writes, deletion,
and rollback when the event cannot commit. Its provider records controlled
results. Existing real Keycloak workflows remain required application checks.
Generated storage tests with a Record entity verify owner isolation, revision
conflicts, transition times, generation changes, deletion, malformed state,
transaction rollback, and schema requirements.
