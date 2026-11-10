This component generates the audit wiring of a service on the audit runtime
(stego.dev/runtime/audit). It generates only what changes with the
declaration:

- the audit settings (level and pii) of the service declaration;
- the sensitive fields of each entity, which the audit rows redact, and
  which entities hold a pii field, whose reads are audited;
- the migration, which runs the DDL text of the runtime with the runtime role,
  the anchor period and the retention;
- the anchor job and the maintenance loop of controller mode, at the anchor
  period of this component;
- the database grants: SELECT and INSERT only on audit_event.

The component requires the audit-sink port, which the platform binds, and
provides no port. The anchor period is a setting of this component
(anchor_period, a Go duration). The retention is retention_months, and the
declaration's audit.retention ("13 months") replaces it.

The stage is alpha. The writer and the sink destination wait on the outbox
adapter of the audit runtime (ST-SUP-RP05-LC14), and the registration of the
jobs in controller mode waits on the periodic job of the modes runtime
(ST-SUP-RP09-LC13).
