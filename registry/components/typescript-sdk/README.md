# TypeScript browser SDK

This component generates a browser client from captured OpenAPI 3.0 contracts.
Select `typescript-sdk` in an archetype or mixin. Set `document` to the root
contract. List its other files in `references`.

The output contains an ES module, TypeScript declarations, and package metadata.
It has no npm runtime dependency. Import `createBrowserClient` from the output.
The client uses the current HTTPS origin and the separate STEGO browser backend.
The browser supplies cookies. Application code does not supply OAuth tokens.

Methods use the contract's `operationId`. Each method accepts an input object
with path and query fields and, where declared, `body`. The optional second
argument supplies an abort signal. Results contain `status`, `body`, and `etag`.
`RequestSchemas` contains request models. `Schemas` contains response models.
Runtime checks enforce the supported schema constraints in each direction.

Call `session()` for public session data. Call `logout()` for console sign-out.
Use the backend's `/auth/logout` page for the provider sign-out confirmation.
The client gets a fresh session CSRF token before each mutation.
Call `login()` to navigate to the same-origin login route. The client retains
a bounded local return path. It does not navigate after an API error unless
the application calls this method.

Declare `error_codes` for public API error codes that the UI needs. Use at most
64 unique names with lowercase letters, digits, and underscores. Each name
starts with a letter and has at most 128 characters. `SDKError.apiCode` contains
only a declared code. Error bodies and private diagnostic fields stay hidden.

See the [contract and test record](../../../specs/typescript-sdk.md) for limits
and open work. Browser telemetry and the full Hypershell UI migration remain
open. The generated Go backend supplies its existing server telemetry.
