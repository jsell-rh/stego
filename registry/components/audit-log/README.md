This component generates the audit wiring of a service on the audit runtime
(github.com/jsell-rh/stego/registry/runtime/audit). It generates only what changes with the
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

The stage is ga. The audit writer of the command and the query runtime writes
each audit row and puts its copy in the outbox (ST-SUP-RP05-LC14). In the
worker mode the outbox worker delivers the copy to the audit-sink port, and in
the controller mode the audit jobs run under the controller lease.
