The compiler now owns the `storage/v1` contract. Standard generation places it
at `out/contracts/storage/storage.go`. It defines resource operations, list
options and results, storage errors, notifications, and transaction interfaces.
The package does not import HTTP, GORM, or a generated internal package.

A component requests a contract version through `Wiring.Contracts`. Assembly
emits each requested version once and includes its module requirements. Unknown
versions fail generation. Component namespaces cannot use `contracts` or its
children. The compiler passes the public import path to each generator through
`Context.StorageContract`.

HTTP handlers and PostgreSQL stores use aliases to the common list types and
errors. Storage no longer imports the HTTP package in standard generation.
Existing HTTP type names remain available. The queue message is an alias of the
public notification type. The contract currently uses UUID message IDs and the
existing generic entity operations. Typed entity schemas and broader protocol
contracts remain open work.

Domain code outside `out` can accept `storage.Transactor`. Its callback receives
`storage.Transaction` and a bounded context. It can perform resource operations
and stage explicit notifications without importing GORM or generated internal
packages. The transaction implementation owns commit and rollback. A store
without an outbox returns `ErrNotificationsUnavailable` from `Notify`. This
also prevents commit when the callback ignores the error.

Direct generator calls with no `StorageContract` retain standalone definitions.
The standard compiler always supplies the public contract path. The standalone
path does not supply the shared domain interface. Tests must use standard
assembly to prove cross-component contract compatibility.

A generated integration service has an application rule under `fills/rules`
and storage and HTTP packages under `out/internal`. The rule imports only the
public contract. PostgreSQL checks show that its resource and notification
commit together. An invalid notification rolls back the resource. A generated
HTTP handler reads the committed resource and maps the shared missing-resource
error to HTTP 404. A Go package import check confirms that storage does not
import the HTTP package. The transaction failure suite also runs through the
public interface, including stores without notification support.

This integration test exposed a constructor reference defect. If a package and
its constructed value had the same name, the compiler renamed the value but
left dependent constructor arguments unchanged. Constructor assembly now keeps
package qualifiers separate from value references. A runtime test checks shared
object identity through a diamond dependency graph. Ambiguous package/value
selectors, affected closures, and uncertain literal keys fail instead of
receiving an unsafe rename.
This does not complete the replacement of string-based wiring with typed
component references.

`ListOptions.Related` now permits filters through declared entity references.
Each filter requires a live related row. The store applies it before count and
pagination through a SQL subquery. Duplicate grants do not duplicate resources.
An empty value list matches no rows. Multiple filters use AND. Unknown entities,
invalid references, unknown fields, and excessive filter sizes fail. Values are
SQL parameters. Field names come from the generated schema. Ordering also checks
the direction at the storage boundary.

PostgreSQL tests use Record and Membership entities to check filtered pages,
totals, duplicate grants, removed grants, empty filters, and hostile input. The
same contract supplies Gateway access filters in the Hypershell test bed. No
entity name or access policy from Hypershell occurs in the filter generator.

Generated handlers do not yet inject domain transaction rules. Typed entity
contracts and REST/gRPC application behavior remain required work.
