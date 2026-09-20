# Controller entry point telemetry audit

This source review covers compiler `c515f22`. It separates runtime ownership
from helper calls. It does not establish complete C6 telemetry coverage.
The [source record](controller-entrypoint-audit-evidence.json) contains the
file hashes and reviewed exported functions.

The table describes generation with the `otel-tracing` peer. The standard
REST, RPC, and browser service archetypes include that component. Generation
without the peer retains the existing no-op controller instrumentation.
These two forms must not be treated as equivalent telemetry evidence.

| Entry point | Current behavior | Evidence limit |
| --- | --- | --- |
| `Main`, through `Monitor` | Owns process telemetry through setup, controller work, and shutdown | This does not add operation records to every callback or helper |
| `Run` | Owns telemetry and records watch, scan, and reconciliation operations | Qualified by the public Run release checks |
| Keyed runners and keyed watch runners, including result variants | Own telemetry and record work, scans, cleanup samples, watch sessions, and queue state | Common scheduling and trace checks cover these paths |
| `RunSweep` | Owns telemetry and records page reads and actions | Sweep checks cover the generated runner |
| `Scan`, `ScanFrom`, and `ScanStream` | Preserve the caller context; do not create a controller telemetry owner or helper operation record | Direct helper use has no separate controller work record |
| Checkpointed and cycle scan helpers | Preserve the supplied context through work and commits; do not create their own controller operation records | The enclosing operation can contain provider spans, but this does not prove separate helper or action signals |
| `RunObservation` | Preserves work, context, and commit failures; does not own telemetry | Its caller supplies the operation context |

Hypershell uses scan helpers inside controller callbacks. It also uses cycle
helpers for identity repair and account cleanup. Existing live evidence checks
the enclosing worker operations and provider calls. It does not establish
standalone helper coverage.

Before changing helper telemetry, define the unit of work and its parent
relationship. Tests must cover direct calls and calls inside an existing
runner. They must retain callback contexts, bounded export and shutdown,
checkpoint commits, retry rules, and failure priority. They must also prevent
duplicate owners or operation records caused by calls between helpers.
These requirements remain open; this audit does not add runtime behavior.
