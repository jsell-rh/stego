# Browser service

This archetype provides the separate Go browser backend, browser telemetry
package, server telemetry, PostgreSQL adapter, health checks, and deployment.
An application does not need a local archetype to add browser telemetry.

The browser client and authenticated backend relay use one service identity.
The default is the service declaration's name. Either component can select an
explicit identity. If both select one, their values must agree. Invalid values
and conflicts fail validation before generated files or state change.

The generated client package is in `out/browsertelemetry` unless the application
selects another output namespace. Add this package to the UI workspace and call
its runtime from the application. Domain event names and attributes remain
application code. Operator telemetry settings control export; adding the
component does not override disabled signals or sampling limits.

The backend keeps OAuth tokens and collector credentials out of browser code.
The relay requires the browser session, Origin, and CSRF checks. Existing body,
queue, admission, and time limits still apply. See the
[browser telemetry contract](../../components/browser-telemetry/README.md).

The browser HTML must have one valid head element for public runtime settings.
Applications supply their assets, API routes, identity-provider settings, and
deployment destinations. These inputs remain subject to the backend and
deployment validation rules.
