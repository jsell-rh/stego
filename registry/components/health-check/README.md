This component generates `/livez` and `/readyz` outside application
authentication. The default readiness check observes only the monitor lifecycle.
Set `database: true` in the service override to include the shared SQL pool.

Checks run in a bounded background loop. HTTP requests read the latest result.
Failed, absent, stale, or canceled observations produce 503. Dependency errors
are not returned to callers. Liveness does not depend on database connectivity.

See [the health contract](../../../specs/health-probes.md) for timing, lifecycle,
security, tests, and limits. Database readiness does not certify the schema,
provider state, or event delivery.
