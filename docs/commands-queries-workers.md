# Commands, queries and workers

This page tells you how to declare commands, queries, workers and
observations in `service.yaml`, and how to write their fills. It also tells
you how to give the protobuf messages of the commands (`proto_files`).

- A **command** is a change that your own code makes in one transaction, such
  as "place an order". The runtime decides the permission of the caller
  before your fill runs.
- A **query** is a read that your own code makes, in a read-only transaction.
- A **worker** is background work that your own code does at an interval, or
  for each message that a command or another worker stages.
- An **observation** is a typed signal that a fill can emit, such as "the
  stock of an item is low".

Each command, query and worker names one fill. The fill is the Go package in
`fills/<fill name>/`. STEGO generates the HTTP handler, the transaction, the
permission decision, the audit row and the typed contract that the fill
implements.

## The example service

The example is the service `inventory`. It has warehouses, stock items and
orders. The create of a warehouse is `public`, with a reason: this is the
choice of the example, because a new warehouse has no parent resource to
decide on. It is not the usual setting. Give each operation the permission
that your schema needs. The example declares:

- The command `place-order`, which reserves stock for a draft order and stages
  a message for the fulfillment worker.
- The command `grant-clerk`, which writes a relationship in SpiceDB.
- The list query `open-orders` and the get query `order-summary`.
- The worker `fulfill-orders`, which consumes the messages of `place-order`.
- The worker `low-stock-scan`, which runs every minute and emits the
  observation `low_stock`.

A test of the STEGO repository runs `stego validate` on this declaration,
with the schema and the proto file below (ST-STD-DOCS-R02). Each smaller
part of a declaration on this page is a part of this declaration.

```yaml
kind: service
name: inventory
archetype: rest-crud
language: go
base_path: /api/inventory/v1
error_type_base: https://errors.example.test/inventory/

tenancy:
  kind: pooled
  key: tenant_id
  claim: tenant
  provisioning: {resource_type: service, provision: provision, retire: retire}
authorization:
  provider: spicedb
  schema_path: authz/schema.zed
  subject_from_identity: user
audit:
  level: change

entities:
  - name: Warehouse
    resource_type: warehouse
    permissions: {create: public, read: view, list: view, update: manage, delete: manage}
    public_reasons: {create: a new warehouse has no resource to decide on before it exists}
    relationships:
      - {relation: manager, subject_source: caller, subject_type: user}
    fields:
      - { name: code, type: string, unique: true, min_length: 2, max_length: 16 }
  - name: StockItem
    resource_type: stock_item
    permissions: {create: public, read: view, list: view, update: edit, delete: manage}
    public_reasons: {create: a new stock item is decided on edit of its warehouse through the reference warehouse_id}
    references:
      warehouse_id: {relation: warehouse, permission: edit}
    fields:
      - { name: warehouse_id, type: ref, to: Warehouse }
      - { name: sku, type: string, min_length: 3, max_length: 40 }
      - { name: quantity, type: int32, min: 0, max: 1000000 }
      - { name: reserved, type: int32, min: 0, max: 1000000, default: 0 }
  - name: Order
    resource_type: order
    permissions: {create: public, read: view, list: view, update: edit, delete: manage}
    public_reasons: {create: a new order is decided on edit of its warehouse through the reference warehouse_id}
    references:
      warehouse_id: {relation: warehouse, permission: edit}
    fields:
      - { name: warehouse_id, type: ref, to: Warehouse }
      - { name: status, type: enum, values: [draft, placed, fulfilled], default: draft }
      - { name: total_units, type: int32, min: 0, max: 1000000, default: 0 }

collections:
  warehouses:
    entity: Warehouse
    operations: [create, read, list]
  warehouse-stock:
    entity: StockItem
    scope: { warehouse_id: Warehouse }
    operations: [create, read, update, list]
  warehouse-orders:
    entity: Order
    scope: { warehouse_id: Warehouse }
    operations: [create, read, list]

commands:
  - name: place-order
    writes: [Order, StockItem]
    resource_type: order
    permission: edit
    resource_member: order_id
    fill: place-order
    route: POST /orders/{order_id}/place
    effects:
      - {name: fulfill, destination: fulfillment}
    refusals:
      - {name: short_stock, code: stock.short, status: 409}
      - {name: not_draft, code: order.not_draft, status: 409}
    lists:
      - name: stock-of-warehouse
        entity: StockItem
        filter:
          - {field: warehouse_id, op: equals, param: warehouse_id}
  - name: grant-clerk
    writes: [Warehouse]
    resource_type: warehouse
    permission: manage
    resource_member: warehouse_id
    fill: grant-clerk
    route: POST /warehouses/{warehouse_id}/clerks
    relationships_written: [warehouse#clerk]

queries:
  - name: open-orders
    reads: [Order]
    resource_type: order
    permission: view
    fill: open-orders
    route: GET /open-orders
  - name: order-summary
    reads: [Order]
    resource_type: order
    permission: view
    resource_member: orderId
    fill: order-summary
    route: GET /orders/{orderId}/summary

workers:
  - name: fulfill-orders
    trigger: consumes fulfillment
    writes: [Order, StockItem]
    resource_type: order
    permission: fulfill
    fill: fulfill-orders
  - name: low-stock-scan
    trigger: every 1m
    writes: [StockItem]
    resource_type: stock_item
    permission: scan
    fill: low-stock-scan
    lists:
      - name: low-items
        entity: StockItem
        filter:
          - {field: quantity, op: less_than, value: 10}

observations:
  - name: low_stock
    kind: event
    severity: warn
    fields:
      - {name: level, type: "enum(low | empty)"}
      - {name: free, type: "measure(1, 0..1000000)"}
    emitted_by: [low-stock-scan]

experimental_acceptances:
  - {component: worker, accepted_until: 2027-03-31, reason: the fulfillment worker reacts to placed orders}

overrides:
  postgres-adapter: { schema_generation: tables-v1 }
  command: { proto_files: [{path: proto/inventory.proto, import_path: inventory.proto}] }
```

The SpiceDB schema is at `authz/schema.zed`:

```zed
definition user {}

definition service {
	relation operator: user
	permission provision = operator
	permission retire = operator
}

definition warehouse {
	relation manager: user
	relation clerk: user
	permission view = manager + clerk
	permission edit = manager + clerk
	permission manage = manager
}

definition stock_item {
	relation warehouse: warehouse
	permission view = warehouse->view
	permission edit = warehouse->edit
	permission manage = warehouse->manage

	relation tenant: tenant
	permission fulfill = tenant->worker_fulfill_orders
	permission scan = tenant->worker_low_stock_scan
}

definition order {
	relation warehouse: warehouse
	permission view = warehouse->view
	permission edit = warehouse->edit
	permission manage = warehouse->manage

	relation tenant: tenant
	permission fulfill = tenant->worker_fulfill_orders
}

definition service_account {}

definition tenant {
	relation worker_fulfill_orders: service_account
	relation worker_low_stock_scan: service_account
}

definition outbox_destination {
	relation operator: user
	permission resolve = operator
}
```

The messages of the commands are at `proto/inventory.proto`:

```proto
syntax = "proto3";

package inventory.v1;

// PlaceOrderRequest places a draft order: it reserves the stock of one item.
message PlaceOrderRequest {
  string order_id = 1;
  string stock_item_id = 2;
  int32 quantity = 3;
}

message PlaceOrderResponse {
  string order_id = 1;
  string status = 2;
  int32 total_units = 3;
}

// GrantClerkRequest makes a user a clerk of a warehouse.
message GrantClerkRequest {
  string warehouse_id = 1;
  string user = 2;
}

message GrantClerkResponse {
  string warehouse_id = 1;
}
```

## The messages of the commands: proto_files

The request and the response of a command are protobuf messages. For the
command `place-order`, STEGO looks for the messages `PlaceOrderRequest` and
`PlaceOrderResponse`: the command name in upper camel case, then `Request` or
`Response`.

Put the messages in one or more proto files in your project. Name each file
in the setting `proto_files` of the component `command`:

<!-- excerpt -->
```yaml
  command: { proto_files: [{path: proto/inventory.proto, import_path: inventory.proto}] }
```

- `path` is the path of the file from the project folder.
- `import_path` is the name that other proto files use to import this file.

The rules:

- Each file is proto3.
- A file holds messages only. It has no `service`. `stego validate` refuses a
  file with a service: `the proto file inventory.proto declares a service;
  the command messages hold messages only`.
- Two files do not declare a message with the same name.
- Do not give `proto_files` when the declaration has no command. Validate
  refuses it.
- You do not need `option go_package`. STEGO puts the Go code of the messages
  in the package `out/commands`, beside the command contract.

The body of a command request is the JSON form of the request message
(protojson). The JSON field names are in lower camel case, such as
`{"stockItemId": "...", "quantity": 2}`. protojson also accepts the proto
field names, such as `stock_item_id`.

### A secret member

Mark each member that holds a secret, such as a PIN in a request or an API
key in a response, with the standard protobuf option `debug_redact`. Without
the mark, STEGO does not know that the member is a secret: the generated CLI
gives it a flag, which puts the value in the shell history and the process
list. `stego validate` warns (`unmarked-secret-member`) of a request member
that has the name of a secret field of an entity that the command writes and
has no mark.

For example, write `string pin = 2 [debug_redact = true];` in the request
message `IssueKeyRequest` and `string api_key = 1 [debug_redact = true];` in
the response message `IssueKeyResponse`. The fill makes the API key with
`command.NewToken(32)` (from runtime v0.163.0), never from an ID or the time
(see [The imports of a fill](fills.md#the-imports-of-a-fill)). A secret in a
response is only for a secret that the command issues: the stored answer
keeps it for the retention of the idempotency key, sealed with the key ring,
and a retry with the same key gets it again. Never put a secret field of an
entity in a response.

From rest-api 4.22.0, cli-application 1.13.0 and command 1.16.0:

- The OpenAPI document gives the member `x-sensitive: true`, and the format
  `password` for a string. A member of a request body is also `writeOnly`.
- The generated CLI gives no flag to a secret request member. Give it in
  `--body FILE`. A command whose answer holds a secret member is Sensitive:
  it writes the answer only to `--output-file FILE`.
- The service reads the key ring (`STEGO_SECRET_KEY_RING_FILE`), as for a
  secret field. The ring keys the fingerprints of the idempotency keys and
  seals the stored answers. Thus a stored API key and the fingerprint of a
  body with a PIN are not in clear in the table `idempotency_keys`.

From rest-api 4.23.0, cli-application 1.15.0 and runtime v0.160.0:

- A command whose answer holds a secret member at any depth is Sensitive,
  also when the member is deeper than the OpenAPI document shows.
- A secret member cannot be a path parameter. Generation refuses the route,
  because the URL goes in access logs. Give the member in the body.
- A Sensitive command refuses `--output-file -`: its answer never goes to
  stdout.
- The CLI refuses a `--body FILE` that holds a secret member when the group
  or other users have access to the file. Use `chmod 600 FILE`.
- The runtime refuses to serve a command with a secret member when the
  service has no key ring.

The mark also works in nested messages, lists and maps. Other tools, such as
the debug output of C++ and Java, also hide a member with this mark.

## Declare a command

<!-- excerpt -->
```yaml
commands:
  - name: place-order
    writes: [Order, StockItem]
    resource_type: order
    permission: edit
    resource_member: order_id
    fill: place-order
    route: POST /orders/{order_id}/place
    effects:
      - {name: fulfill, destination: fulfillment}
    refusals:
      - {name: short_stock, code: stock.short, status: 409}
      - {name: not_draft, code: order.not_draft, status: 409}
```

| Key | What it is |
|---|---|
| `name` | The name, in lower case with hyphens. It is unique among the commands, queries and workers. |
| `writes` | The entities that the fill can read and write. The fill cannot reach other entities. |
| `resource_type` | The SpiceDB type of the row on which the runtime decides the permission. One entity in `writes` has this type. |
| `permission` | The permission that the caller must have on that row. |
| `resource_member` | The field of the request message that holds the ID of that row. |
| `route` | `POST` and a path under `base_path`. |
| `fill` | The name of the one fill. |
| `refusals` | The refusals that the fill can return. Optional. |
| `effects` | The messages that the fill can stage, each with a destination and a name. Optional. |
| `relationships_written` | The SpiceDB relations that the fill can write, as `resource_type#relation`. Optional. |
| `retry_on_conflict` | How many times the runtime runs the fill again after a write conflict: 0 to 3. The default is 0. |
| `lock_timeout` | How long a statement waits for a row lock: from `100ms` to `10s`. The default is `2s`. |

### The route and the resource member

Each path parameter of the route is a string field of the request message.
The runtime copies the path value into that field. The status of a success
comes from the last segment of the path:

- When the last segment is a parameter, such as `/orders/{order_id}`, or has
  a colon, such as `/orders/{order_id}/stock:reserve`, the command answers 200.
- When the last segment is a plain word, such as `place` in
  `/orders/{order_id}/place`, the command creates a resource and answers 201.

The resource member is a string field of the request message. Give the proto
field name (`order_id`) or the JSON name (`orderId`). Both are accepted.

Before the fill runs, the runtime reads the row with this ID and decides the
permission of the caller on it. If the caller does not have the permission,
the answer is 404, and the fill does not run.

Note: a query takes its resource member in lower camel case only. See
[Declare a query](#declare-a-query).

### A command with no resource member

A command whose `resource_type` is the resource type of the tenant
provisioning, such as `service`, and that names no `resource_member`, creates
rows with no parent. The runtime decides its permission on the service-level
resource of the tenant of the request. Its SpiceDB ID is tenant-qualified:

```text
service:<base64url of the tenant ID>/<base64url of the service name>
```

base64url has no padding. For the tenant `acme` of the service `fulfilment`,
the ID is `service:YWNtZQ/ZnVsZmlsbWVudA`. A grant on `service:fulfilment`,
with no tenant, is not a grant in a tenant: the command answers 404. Only
Provision and Retire are decided on `service:<service name>`.

SpiceDB knows a person by the user ID
`<base64url of the issuer>|<base64url of the sub claim>`.

The first grant in a new tenant comes from Provision. Declare the relation
with `admin` in `tenancy.provisioning`, such as
`{resource_type: service, provision: provision, retire: retire, admin: admin}`.
Then Provision takes the first administrators of the tenant in its body:

```bash
curl -X POST https://<host>/api/inventory/v1/stego/tenants \
  -H "Authorization: Bearer $OPERATOR_TOKEN" \
  -H 'Idempotency-Key: "provision-acme"' \
  -d '{"tenant_id":"acme","admins":[{"subject":"dora"}]}'
```

Provision writes the grant `admin` of each person on the tenant-qualified ID,
in its transaction. The service serves these tenant routes under its base
path:

| Route | What it does | Permission on `<resource type>:<service name>` |
|---|---|---|
| `POST <base path>/stego/tenants` | Provision: adds the tenant, with its first admins and workers. | `provision` |
| `POST <base path>/stego/tenants/{tenantId}/retire` | Retire: stops the tenant. The rows stay. | `retire` |
| `GET <base path>/stego/tenants` | Lists the tenants, with `limit` and `cursor`. Served only when the schema declares `permission list_tenants`. | `list_tenants` |

For example, to retire the tenant `acme`:

```bash
curl -X POST https://<host>/api/inventory/v1/stego/tenants/acme/retire \
  -H "Authorization: Bearer $OPERATOR_TOKEN" \
  -H 'Idempotency-Key: "retire-acme"'
```

A retire is for good. The tenant ID stays in the tenant list as retired, so
a later provision of the same ID answers 409 with the code `tenant.retired`,
and no route, worker or subscription of the service acts in that tenant
again. (A provision of an ID that is active answers 409 with the code
`tenant.provisioned`.) To serve the same customer again, provision a new
tenant ID. A retire changes only the service that gets it: retire the tenant
in each service of the chain.

The
[rpc-service guide](../registry/archetypes/rpc-service/README.md#tenants-and-the-first-grant)
gives the rules of `admins`, the operator's own grant and how to write later
grants.

### Idempotency

Each request of a command must have the header `Idempotency-Key`.

- With no key, the answer is 400 with the problem type
  `idempotency-key-missing`.
- With a key that is not one structured string of 1 to 255 characters, such
  as `abc` with no double quotes, the answer is 400 with the problem type
  `idempotency-key-invalid`. Put the key in double quotes, such as `"k-1"`.
- A request again with the same key, method, path and body gets the stored
  response again. The fill does not run again.
- A request with the same key and another body gets 422 with the problem type
  `idempotency-key-reused`.
- A request with the same key while the first request runs gets 409 with the
  problem type `idempotency-key-in-progress`.

The keys of each tenant and each caller are separate. Thus the same key from
another caller, or in another tenant, is a new request.

The header `Idempotency-Kept: 1` is for HTTP only. A command over gRPC gets no
such metadata (ST-SUP-Q199 (d)). After a failure, a gRPC client sends the
command again only with the same key: the service then gives the stored
answer, or runs the command once.

The runtime keeps only an answer that commits. It does not keep an error
answer: a 404, a 400, a declared refusal, an answer of 5xx, an answer of
429, or an answer 409 with the code `conflict`. For each of these, it rolls
back the transaction of the command, and the key with it. A request again
with the same key then runs the fill again, and can get another answer. The
rows that the fill writes and the effects that it stages in the
outbox are in that transaction, so only the run that commits keeps them.
One answer is not an error in this sense: a before slot of a collection that
halts the command (its result sets `Halt` with a status). The command then
commits with no write, and the runtime keeps the answer of the halt with the
key, also when its status is a 4xx. A request again with the same key gets
that stored answer, and the slot does not run again. But a halt with a status
of 500 or more, or 429, is not kept: the command rolls back as for an error
answer, and a request again with the same key runs the slot again. A before
slot that refuses the request (its result is not `Ok`) rolls back as an error
does.
Anything that the fill does outside the transaction, such as a log line,
occurs again at each request. It can also occur twice in one request: the
runtime runs a command again once, in a new transaction, when the database
refuses a prepared statement that a change of a table made stale
(ST-SUP-RP04-LC11). Thus, send each change to another system as an
effect through the outbox (see [Effects](#effects)), never from the fill
directly. A worker then sends it after the commit.

A worker can send one effect more than one time, for example after a crash
before it records that the send is done (see [The fill of a
worker](#the-fill-of-a-worker)). Thus the system that gets the effect must
find a second copy and ignore it: give the effect a stable ID in its
payload, such as the ID of the row and the change, and let the receiver keep
the IDs that it applied.

The runtime keeps each key for the time in `idempotency.retention`. The
default is `24h`.

To send the same answer again, the runtime keeps the whole answer of each
key: its status, its headers and its body. The body holds the fields of the
row, also a `pii` field, such as an email. From runtime v0.138.0, a service
with a `pii` or a `secret` field seals the headers and the body of each
stored answer with its key ring, with the tenant, the principal and the key
as the place of the sealed value (ST-SUP-Q182 (b)). A replay opens them and
gives back the first answer byte for byte. Such a service needs its key ring
(see [Rotate and back up the key ring of the secret
fields](operations.md#rotate-and-back-up-the-key-ring-of-the-secret-fields)).
A service with no `pii` and no `secret` field keeps its answers in clear for
the retention of the key, 24 hours by default: such an answer holds no
personal data. Do not make the retention longer than your clients need.

A key row that a release before v0.138.0 stored in clear does not match a
retry of a release that seals, and a sealed row does not match a retry of a
release before: the retry gets `idempotency-key-reused`, and the client sends
it again with a new key. The runtime never answers the sealed bytes as a
body.

### Refusals

A refusal is an answer that the fill gives when a business rule stops the
command. Each refusal has:

- `name`, in lower snake case.
- `status`: 400, 403, 404, 409, 412 or 422.
- `code`: a fixed code, such as `stock.short`. The codes `internal` and
  `conflict` are reserved.

The contract has one value for each refusal, named with the command and the
refusal: `commands.PlaceOrderShortStock` and `commands.PlaceOrderNotDraft`.
The fill returns this value as its error. The handler then answers with the
status and the code of the refusal. From runtime v0.139.0, the error
`command.ErrNotFound`, which a read of the handle gives for a row that is not
there or that the caller may not read, is 404 with the code `not_found`, and
`NotFound` over gRPC. Any other error is 500 with the code `internal`, and the
transaction is rolled back. The log then has one line `command.fill.failed`
with the command and a fixed cause class, and no error text. To tell the
client which ID is wrong, map `command.ErrNotFound` to a declared refusal, as
[the developer reference of fills](fills.md#map-an-error-of-the-handle-to-a-refusal)
shows.

### Effects

An effect is a message that the fill stages in the outbox, in the transaction
of the command. A worker that consumes the destination gets the message after
the commit. The fill stages it with `h.Effects().Stage`:

```go
err := h.Effects().Stage(ctx, "fulfillment", "fulfill", updated, payload)
```

The arguments are the destination, the effect name, the loaded row that the
message is about, and the payload as JSON.

### A relationship that a command writes

The command `grant-clerk` makes a user a clerk of a warehouse:

<!-- excerpt -->
```yaml
  - name: grant-clerk
    writes: [Warehouse]
    resource_type: warehouse
    permission: manage
    resource_member: warehouse_id
    fill: grant-clerk
    route: POST /warehouses/{warehouse_id}/clerks
    relationships_written: [warehouse#clerk]
```

Each relation in `relationships_written` is a relation of the SpiceDB type of
an entity that the command writes. A command cannot write the relation
`tenant`, a relation `worker_<name>`, or a relation that the runtime writes
itself.

For each relation, the contract has two helpers:
`commands.GrantClerkRelateClerk` and `commands.GrantClerkUnrelateClerk`. Each
takes a loaded row of the handle, the subject type and the subject ID. The
runtime writes the tuple to SpiceDB after the commit.

The runtime forms the SpiceDB ID of a user as the base64url form of the
issuer, a `|`, and the base64url form of the `sub` claim. The contract has no
helper that makes this ID for another user. The fill of the example makes it
from the issuer part of the caller's own ID.

### The fill of a command

The fill package exports `New`, which gives the fill function of the command
type `commands.<Command>Fill`. This is a part of the fill of `place-order`:

```go
// New gives the fill of place-order.
func New() commands.PlaceOrderFill {
	return func(ctx context.Context, req *commands.PlaceOrderRequest, h *commands.PlaceOrderHandle) (*commands.PlaceOrderResponse, error) {
		orders, err := commands.OrderRepository(h)
		if err != nil {
			return nil, err
		}
		order, err := orders.GetForUpdate(ctx, req.OrderId)
		if err != nil {
			return nil, err
		}
		if order.Value().Status != "draft" {
			return nil, commands.PlaceOrderNotDraft
		}
		// Reserve the stock, update the order and stage the effect.
	}
}
```

The handle gives a repository for each entity in `writes`, such as
`commands.OrderRepository(h)`. It gives no database, no network client and no
other entity. [The developer reference of fills](fills.md) gives each method of
a repository and the decision that it makes.

### The lists of a command

A command fill reads a set of rows through a declared list, as a worker fill
does (ST-DECL-INV94). `place-order` reads the stock items of the warehouse of
its order. The warehouse comes from the order, so the term gives a parameter
in place of a value:

<!-- excerpt -->
```yaml
    lists:
      - name: stock-of-warehouse
        entity: StockItem
        filter:
          - {field: warehouse_id, op: equals, param: warehouse_id}
```

`stego apply` writes the list `commands.PlaceOrderStockOfWarehouse` in
`out/commands`. Because the list has a parameter, it also writes the function
`commands.PlaceOrderStockOfWarehouseWith(warehouseID string)`. The fill gives
the value of the parameter to this function:

```go
items, err := commands.StockItemRepository(h)
if err != nil {
	return nil, err
}
stock, err := items.List(ctx, commands.PlaceOrderStockOfWarehouseWith(order.Value().WarehouseID), 100)
```

The generator builds the statements of the list from the entity of the
release. When a schema change adds or renames a field, the next release
writes the list again with the new columns. The handle binds each parameter as
a SQL parameter, never as text. A parameter of a reference field is a key: a
value that the key check of the entity refuses gives an empty list. A command
on the service-level resource has no list.

A list of a child entity reads only the rows of one decided parent. Its parent
term is `{field: order_id, op: equals, parent: true}`, and its function takes the
loaded parent row, such as `OfOrderWith(order LoadedOrder)`. The handle takes
only a row that this handle read. Before it reads a row, it decides the
permission of the reference on the parent, so a caller that may act on one
order never lists the rows of another order.

## Declare a query

<!-- excerpt -->
```yaml
queries:
  - name: open-orders
    reads: [Order]
    resource_type: order
    permission: view
    fill: open-orders
    route: GET /open-orders
  - name: order-summary
    reads: [Order]
    resource_type: order
    permission: view
    resource_member: orderId
    fill: order-summary
    route: GET /orders/{orderId}/summary
```

| Key | What it is |
|---|---|
| `name` | The name, in lower case with hyphens. |
| `reads` | The entities that the fill can read. |
| `resource_type` | The SpiceDB type of the rows that the query reads. One entity in `reads` has this type. |
| `permission` | The permission that the caller must have on each row. |
| `resource_member` | For a get query only: the request member that holds the ID of the one row. |
| `route` | `GET` and a path under `base_path`. |
| `fill` | The name of the one fill. |
| `refusals` | The refusals that the fill can return, as for a command. Optional. |

### A get query and a list query

A query with a `resource_member` is a **get query**. It reads one row. A query
with no `resource_member` is a **list query**. It reads one page of rows.

For a get query:

- The resource member is in lower camel case, such as `orderId`. A name with
  an underscore, such as `order_id`, is refused: `the query order-summary
  names the resource member "order_id", which is not a member name in lower
  camel case`.
- The only path parameter that the route can have is the resource member. If
  the route has no parameter, the request gives the ID in the URL query, such
  as `?orderId=...`.
- The runtime decides the permission on the row before the fill runs.

For a list query:

- The route has no path parameter. A path parameter is refused: `the query
  open-orders has the path parameter {warehouseId}, which is not its resource
  member`. Thus a list query cannot take the ID of a parent from the path. The
  request of a list query holds only the cursor and the page size, so the fill
  filters the rows only by values that the fill itself sets.
- The runtime keeps only the rows on which the caller has the permission.
- The request pages with `limit` and `cursor`. See [Paging](#paging).

A list query whose `method` is a gRPC server stream, such as `rpc
ListAccounts(ListAccountsRequest) returns (stream Account)`, is a **stream
query**. It is served over gRPC only: the stream sends each row that the
caller may read, and has no page. A stream query declares no `route`.
Checking refuses a route on it: `query "list-accounts" has the route "GET
/accounts" and the gRPC method billing.v1.Billing/ListAccounts, which is a
server stream; a stream is served over gRPC only, so remove the route`. To
list the same rows over HTTP, declare a second list query with a `route` and
a unary method, or with no method. The Product owner decided that an HTTP
stream waits until an HTTP client needs one (ST-SUP-RC33-OC08). A route that
no handler serves would answer 404 or 405 with no fault, so checking refuses
it.

### The fill of a query

The fill package exports a function with the name `Fill` of the type
`queries.<Query>Fill`. It does not export `New`. This is the fill of
`open-orders`, with no change:

```go
// Fill lists one page of the placed orders.
func Fill(ctx context.Context, req queries.OpenOrdersRequest, h *queries.OpenOrdersHandle) (queries.OpenOrdersResponse, error) {
	orders, err := queries.OpenOrdersOrders(h)
	if err != nil {
		return queries.OpenOrdersResponse{}, err
	}
	page, err := orders.List(ctx, queries.ListRequest{Limit: req.Limit, After: req.After,
		Where: []query.Equal{{Field: "status", Value: "placed"}}})
	if err != nil {
		return queries.OpenOrdersResponse{}, err
	}
	items := make([]Item, 0, len(page.Items))
	for _, o := range page.Items {
		v := o.Value()
		items = append(items, Item{ID: o.ID(), WarehouseID: v.WarehouseID, TotalUnits: v.TotalUnits})
	}
	return queries.OpenOrdersResponse{Items: items, Next: page.Next}, nil
}
```

`query` is the package `github.com/jsell-rh/stego/registry/runtime/query`, and
`Item` is a type of the fill. The handler writes `Items` as JSON.

The request of a get query has one field for the member: `req.OrderID` for
`orderId`. The response of a get query is any value that the handler can
write as JSON.

Do not put a secret in the answer of a query. The read-only value of an
entity has no `secret` field, so a query fill cannot read a stored secret.
The OpenAPI document gives a query answer as any JSON value, with no
`x-sensitive` mark, and the generated CLI writes it to stdout. A call that
makes a secret, such as a new API key, is a command with a `debug_redact`
member in its response message (see [A secret member](#a-secret-member);
ST-SUP-Q199 (a)).

### An optional field in a command and in a query

The Go type of an optional field is not the same in the two packages:

- In `out/commands`, an optional field that is not bytes or jsonb is a
  pointer, such as `*string`. A command writes the row, so `nil` sets the
  column to NULL, and a pointer to `""` sets it to the empty text.
- In `out/queries`, the value that a fill reads has the field as a value,
  such as `string`. A NULL column gives the zero value: `""`, `0` or `false`.

Thus a command fill uses `v.Carrier != nil` and `*v.Carrier`, and a query
fill uses `v.Carrier` with no `*`. Do not copy the code that reads a field from
a command fill into a query fill.

The rest-api handlers read the type `<Entity>View`, in which an optional field
is a pointer, so an answer leaves out a NULL field (decision ST-SUP-Q128,
option B). The value that a query fill reads did not change, so no fill had to
change. Thus a query fill cannot tell NULL from the zero value. A pointer in
the query value needs a major version of each archetype that has a query
(ST-SUP-Q128, option A), and it is not built.

## Paging

A collection list and a list query page in different ways:

| | Collection list | List query |
|---|---|---|
| Page size parameter | `pageSize` | `limit` |
| Default page size | 100 | 100 |
| Largest page size | 1000. A larger value gives 1000. | 1000. A larger value gives 400. |
| Value less than 1 | 400 | 400 |
| Next page | `cursor` | `cursor` |
| Response | `items` and `next` | `items` and `next` |

Each parameter name is exact. A collection list reads only `pageSize`,
`cursor`, `orderBy`, `fields` and `search`. It answers 400 for each other
parameter, such as `limit` or `PageSize`, and the detail names the parameter
(rest-api 4.14.0). A list query ignores `pageSize`. Give the `next` value of a response as the `cursor`
of the next request. A cursor is valid only for the same list, tenant and
caller. Another cursor gives 400.

## Commands and queries in the OpenAPI document

From rest-api 4.17.0, the OpenAPI document of the service
(`out/internal/api/openapi.json`, also served at its discovery route) holds
each command and each get or list query that the service serves over HTTP, at
its route. Its operationId is the name in lower camel case, such as
`dischargePatient`. A command has its path parameters, the required header
`Idempotency-Key`, the request and the response message in their protobuf
JSON form, and a response for each refusal. A list query has `cursor` and
`limit`. A stream query is served over gRPC only, so it is not in the
document.

The protobuf JSON form names each member in lower camel case, such as
`closedRecords`, and gives a 64-bit integer as a string. A request can give a
member by either name, such as `insurance_number` or `insuranceNumber`. Thus
the request validation of the document does not check the body of a command;
the command runtime decodes it.

A service with no rest-api component, such as an rpc-service, has no such
document; the command component writes the document of its command and query
routes, in the same form, in `out/commands/openapi.json`.

The generated clients come from this document. Bind the mixin `go-sdk` for a
Go client (`out/sdk`) and the mixin `cli-application` for a CLI
(`out/cli/cmd`), with `mixins: [go-sdk, cli-application]`. See the READMEs of [go-sdk](../registry/components/go-sdk/README.md) and
[cli-application](../registry/components/cli-application/README.md).

## Declare a worker

<!-- excerpt -->
```yaml
workers:
  - name: fulfill-orders
    trigger: consumes fulfillment
    writes: [Order, StockItem]
    resource_type: order
    permission: fulfill
    fill: fulfill-orders
  - name: low-stock-scan
    trigger: every 1m
    writes: [StockItem]
    resource_type: stock_item
    permission: scan
    fill: low-stock-scan
    lists:
      - name: low-items
        entity: StockItem
        filter:
          - {field: quantity, op: less_than, value: 10}
```

| Key | What it is |
|---|---|
| `name` | The name, in lower case with hyphens. |
| `trigger` | `every <interval>`, in whole seconds, minutes or hours (`s`, `m`, `h`) from `10s` to `24h`, or `consumes <destination>`. |
| `writes` | The entities that the fill can read and write. |
| `resource_type` | A SpiceDB type of the rows that the worker reads and writes. |
| `permission` | The permission that the worker needs on each row that it reads or writes. |
| `fill` | The name of the one fill. |
| `effects` | The messages that the fill can stage, as for a command. Optional. |
| `single` | For an `every` worker: when true, one instance of the service runs the worker at a time, under a lease. The default is true. |
| `lease_ttl` | The time that the lease holds, from `15s` to `5m`. The default is `15s`. |
| `lists` | The lists of rows that the fill reads. See [The lists of a worker](#the-lists-of-a-worker). Optional. |

The component `worker` is at the stage alpha. Accept it in the declaration
with a date before its sunset (2027-04-30) and a reason:

<!-- excerpt -->
```yaml
experimental_acceptances:
  - {component: worker, accepted_until: 2027-03-31, reason: the fulfillment worker reacts to placed orders}
```

### The two triggers

- `every 1m`: the runtime runs the fill once in each tenant at each tick, in
  one transaction of that tenant. The ticks are at the multiples of the
  interval since the Unix epoch, in UTC: `every 1h` runs at the start of
  each hour, and `every 24h` at 00:00 UTC. The first tick is the first such
  time after the worker starts, not the start: an `every 24h` worker that
  starts at 10:00 UTC runs first at 00:00 UTC the next day. A tick that is
  due while the service is stopped, or while a tick of the worker runs, is
  not run later.
- `consumes fulfillment`: the runtime runs the fill once for each message of
  the destination `fulfillment`, in the transaction of the tenant of the
  message. A command or a worker stages these messages as an effect.

### A job that runs once a month

The longest interval of `every` is `24h`. A worker has no calendar trigger,
such as "on the first day of each month". For a job of each month, run the
worker each hour, and let the fill find out if the month has a run already:

1. Declare the worker with `trigger: every 1h`.
2. Declare an entity that records each run, with the period as a field, such
   as `MonthlyClose` with `period` (`pattern: "^[0-9]{4}-[0-9]{2}$"`). Put it
   in `writes`.
3. Declare in the worker a list of the runs of one period: the list
   `closes-of-period` of the entity `MonthlyClose`, with the filter term
   `{field: period, op: equals, param: period}`.

4. In the fill, find the period from the tick, which is the time of the run.
   Return with no change when the list holds a row of the period. Else do the
   job, and create the row of the period in the same transaction:

   ```go
   first := time.Date(tick.Year(), tick.Month(), 1, 0, 0, 0, 0, time.UTC)
   period := first.AddDate(0, -1, 0).Format("2006-01") // the month before the tick
   closes, err := workers.MonthlyCloseRepository(h)
   if err != nil {
   	return err
   }
   done, err := closes.List(ctx, workers.MonthlyCloseClosesOfPeriodWith(period), 1)
   if err != nil || len(done) > 0 {
   	return err
   }
   // Close the month here, then record it.
   _, err = closes.Create(ctx, workers.MonthlyClose{Period: period})
   return err
   ```

The first run of each month closes the month before it. When the service is
stopped at the start of a month, the first run after the start does the
job. A run that fails commits nothing, so the next run does the job again.
Each run reads one list in each tenant, which costs little. Use the first day
of the month in UTC, as above, and not `tick.AddDate(0, -1, 0)`: on 31 March
that gives 3 March. A daily job is `every 24h`. A worker that must run at one
time of the day uses the same form: `every 1h` and a check of the hour.

### The permission of a worker

A worker acts as the service account `service_account:<worker name>`, not as
a user. Each tenant gives this account one relation on the SpiceDB type
`tenant`. The runtime writes these tuples. See
[Worker grants in each tenant](workers.md).

Do these steps in the schema for each worker:

1. Give the type `tenant` the relation `worker_<worker name in snake case>`
   of the subject type `service_account`:

   <!-- excerpt -->
   ```zed
   definition tenant {
   	relation worker_fulfill_orders: service_account
   	relation worker_low_stock_scan: service_account
   }
   ```

   Validate refuses a schema without this relation.

2. Give each type of row that the worker reads or writes the relation
   `tenant` and the permission that the worker names, through the worker
   relation. The runtime relates each row to its tenant when it creates the
   row.

   <!-- excerpt -->
   ```zed
   	relation tenant: tenant
   	permission fulfill = tenant->worker_fulfill_orders
   ```

   The worker `fulfill-orders` writes `Order` and `StockItem`, so the types
   `order` and `stock_item` both have the permission `fulfill`.

Validate examines the permission on `resource_type` only. If another type in
`writes` does not give the permission through the worker relation, the
runtime refuses each read and write of the worker on those rows.

3. When the worker creates a row with no parent, give the type `tenant` the
   permission of the worker too. A row has no parent when its entity has a
   resource type and no required reference to an entity with a resource type,
   such as the local copy of an event of another service. The runtime decides
   such a create on the tenant resource with the permission of the worker. A
   consuming worker also needs the permission of the stager there.

   The SpiceDB ID of the tenant resource is tenant-qualified, as each other ID
   of a tenant is: `tenant:<base64url of the tenant ID>/<base64url of the
   tenant ID>`, with no padding. For the tenant `acme`, it is
   `tenant:YWNtZQ/YWNtZQ`. A grant on `tenant:acme` is not a grant in the
   tenant `acme`, and it gives nothing. This command gives the ID:

   ```bash
   b64() { printf %s "$1" | base64 | tr '+/' '-_' | tr -d '=\n'; }
   TENANT=acme
   t=$(b64 "$TENANT"); echo "tenant:$t/$t"
   ```

   For a worker `record-orders` with the permission `record`, the type
   `tenant` holds `relation worker_record_orders: service_account` and
   `permission record = worker_record_orders`.

   Generation refuses a schema whose type `tenant` has no such permission,
   and validate gives the warning `worker-root-create` before that.

   A person reads such a row only through a relation of the type `tenant`.
   Give the type `tenant` the relation `service` of the provisioning resource
   type, such as `relation service: service`. Then each Provision relates the
   tenant resource to the service-level resource of the same tenant, which
   holds the admins of the Provision, and each Retire deletes the relation
   (ST-SUP-RC27-OC18). The schema then decides who reads the row, such as
   `permission audit = service->admin` on the type `tenant` and
   `permission view = tenant->audit` on the type of the row. Validate refuses
   the relation `service` when the declaration has no tenant provisioning, or
   when the relation admits another subject type. See [the developer
   reference of fills](fills.md#rows-with-no-parent-and-who-reads-them).

### The fill of a worker

The fill package exports `New`, as for a command. The type of the fill
function depends on the trigger.

A consuming worker gets the message as JSON and the effect name:

```go
// New gives the fill of fulfill-orders.
func New() workers.FulfillOrdersFill {
	return func(ctx context.Context, message json.RawMessage, effect string, h *workers.FulfillOrdersHandle) error {
		var f Fulfillment
		if err := json.Unmarshal(message, &f); err != nil || effect != "fulfill" {
			return workers.Refused(ErrBadMessage)
		}
		// Read the order and the stock item, then update them.
	}
}
```

- When the fill returns nil, the transaction commits.
- When the fill returns `workers.Refused(err)`, the runtime parks the message.
  It does not deliver the message again.
- When the fill returns another error, the transaction is rolled back and the
  runtime delivers the message again. Thus a fill can get one message two
  times. Make the fill safe for this: `fulfill-orders` does nothing for an
  order that is not `placed`.

A worker writes rows through the repository of each entity in its `writes`,
as a command does. To create a row with no parent, such as the local copy of
an event of another service, give `Create` the value alone:

```go
accounts, err := workers.AccountRepository(h)
if err != nil {
	return err
}
_, err = accounts.Create(ctx, workers.Account{ProjectRef: e.ID, Plan: "free"})
```

The runtime decides that create on the tenant resource with the declared
`permission` of the worker (ST-SUP-Q164). Thus the definition of the tenant in
the schema must give that permission to the worker relation, such as
`permission bill = worker_bill_projects`. Generation refuses a schema that
does not. To create a row with a parent, give the loaded parent after the
value, such as `charges.Create(ctx, charge, account)`, where `account` is a row
that the fill read through its repository.

An interval worker gets the time of the tick:

```go
// New gives the fill of low-stock-scan.
func New() workers.LowStockScanFill {
	return func(ctx context.Context, tick time.Time, h *workers.LowStockScanHandle) error {
		// Read the stock items of this tenant and emit low_stock for each low item.
	}
}
```

### The lists of a worker

A worker fill reads a set of rows through a declared list (ST-DECL-INV94).
`low-stock-scan` reads the stock items with fewer than 10 units:

<!-- excerpt -->
```yaml
    lists:
      - name: low-items
        entity: StockItem
        filter:
          - {field: quantity, op: less_than, value: 10}
```

| Key | What it is |
|---|---|
| `name` | The name of the list, unique in its worker. |
| `entity` | An entity that the worker writes. |
| `filter` | One to eight terms. Each term has a `field` of the entity and an `op`. |
| `op` | `equals`, `not_equals`, `less_than`, `less_than_or_equals`, `greater_than`, `greater_than_or_equals` or `is_set`. |
| `value` | A constant of the field type. `is_set` takes no value. |
| `now` | `true` in place of `value`: the time of the run. This is the time of the tick of an `every` worker, and the start of the run of a consuming worker. Use it only on a timestamp field. |
| `param` | A name in lower snake case, in place of `value`: a parameter whose value the fill gives at call time, such as a field of the message. Each parameter has its own name in the list. `is_set` takes no parameter. |
| `parent` | `true` in place of `value`, with `equals`, on the reference of a child entity: the fill gives the loaded parent row, such as a `LoadedOrder`. A child entity has no resource type and a reference to an entity with one. A list of a child needs exactly one parent term, and a list of any other entity has none. |
| `lock` | Optional. When true, `ListForUpdate` locks the rows. The entity must declare `lock_at_most`, a whole number from 1 to 1000. |

`stego apply` writes one typed list for each declared list in `out/workers`.
When a generated file of `out/workers` is missing, `stego apply` writes it
again with no flag; it refuses a generated file that was edited by hand.
Its name is the worker name and the list name in mixed case, such as
`LowStockScanLowItems`. The generator builds the statements of the list from
the entity of the release: the columns of the entity, the tenant, and the
filter as bound values. When a schema change adds a field, the next release
writes the list again with the new column, and the release before it still
reads its own columns.

The fill reads the list through the repository of the entity:

```go
items, err := workers.StockItemRepository(h)
if err != nil {
	return err
}
low, err := items.List(ctx, workers.LowStockScanLowItems, 100)
```

A declared list takes no filter values from the fill. A list with a parameter
has a generated function `<Worker><List>With`, which takes each parameter as a
typed Go value, in the order of the terms. A consuming worker can give a field
of its message as a parameter.

A list keeps only the rows on which the worker has its permission in SpiceDB
(ST-SUP-RP10-LC17). Thus a row with no tuple is not in the list: a row that a
person or a script wrote past the service, or a new row whose tuples the relay
did not write yet. A worker that checks a rule over all the rows, such as a
sum of the balances, does not see such a row. Write the rows only through the
service. [The developer reference of fills](fills.md) gives the decision of
each method of the handle.

### A failed run of a consuming worker

Each attempt of a consuming worker that does not commit writes one log line
`command.consumer.run_failed` at the level warn. The line gives the worker, the
destination, the event ID of the message, the attempt number, the outcome and
the cause class. It never gives the error text or a value of the message. The
attempt is also sent as `worker.run.failed` with the worker, the outcome and
the cause class.

An error of the fill that is not an error of the handle gets one of these
cause classes. The runtime reads the type of each error in the chain of the
error (`errors.As`), never its text:

| Cause | What the chain of the error holds |
|---|---|
| `fill_json` | An `encoding/json` `SyntaxError`, `UnmarshalTypeError` or `InvalidUnmarshalError`, such as from a decode of the payload. |
| `fill_parse` | A `strconv` `NumError` or a `time` `ParseError`. |
| `fill_canceled` | `context.Canceled`. |
| `fill_error` | Another error. |

The text of a fill error can hold a value of the message or of a row, so no
log line and no observation gives it. To tell an operator more, the fill logs
its own context with `log/slog`, which the fill allow list takes, before it
returns the error: names and counts, never a value. For example:

```go
	if err := json.Unmarshal(payload, &reply); err != nil {
		slog.WarnContext(ctx, "apply-partner-replies: the reply does not decode", "bytes", len(payload))
		return fmt.Errorf("decode the reply: %w", err)
	}
```

When the fill fails with the same outcome and cause class in 5 attempts in a
row, the runtime parks the message as poison (failure code `poison`) and
raises the alert of a parked message. Only the errors that the fill causes are
counted: an error of the fill (`fill_error`, `fill_json`, `fill_parse` and
`fill_canceled`, but not a fill canceled because the worker stops), a row that is not there, an invalid value, a
refused resource, a unique violation and a panic. A provider that cannot decide
now, a conflict, a failed database statement, a timeout and a lost connection
are not counted, and they start the count again. A parked message stays in the
outbox, and a person can send it again or discard it. Until a person resolves
it, a parked message holds back each later message of the same destination,
tenant and resource key, so the later events of the same row wait.

The log line and the alert of a parked message are also written to standard
output as one JSON line each, also when no OpenTelemetry collector is set. The
runtime writes each log record at the level warn or higher in this way, at most
10 lines of one event name in each minute, and then one line with the event
name and the number of lines that it dropped.

### See and resolve the parked messages

In the api mode, the service serves two routes for an operator, behind the
same authentication as the other routes:

- `GET <base path>/stego/outbox/{destination}/parked` lists the parked
  messages of the destination in the tenant of the caller: the message ID, the
  destination, the failure code, the attempt count and the times when the
  message was made and parked. It never gives the payload. It pages with the
  `limit` and `cursor` parameters, as the other lists do.
- `POST <base path>/stego/outbox/{destination}/parked/{id}:resolve` with the
  body `{"resolution": "not_acted"}` resolves one message. `not_acted` sends
  it again with the same identity, `acted` marks it done, and `discarded`
  discards it. The answer is 204, and the audit row
  `resolve-parked-message` is written in the same transaction.

The destination decides which resolutions it accepts (ST-SUP-RP01-LC13):

- An idempotent destination refuses `acted` with 409. To send such a message
  again is safe, and `acted` would lose what the delivery writes. These
  destinations are idempotent: `authz`, the audit sink, the change events, a
  subscription that a worker consumes, and each destination of an effect whose
  component declares it with `idempotent: true` in its setting
  `destinations`. For them, correct the cause and send the message again with
  `not_acted`, or use `discarded`. A destination with `idempotent: false`
  accepts `acted`: use it when the receiver already did the work.
- A destination that need not land refuses `discarded` with 409.

The caller needs the permission `resolve` on `outbox_destination:<destination>`
in its tenant. Declare the type in the authorization schema, as the schema of
this page does:

<!-- excerpt -->
```zed
definition outbox_destination {
	relation operator: user
	permission resolve = operator
}
```

A caller without the permission gets 403, the same for each destination and
each message. A message that the tenant does not hold gets 404. A message that
is not parked, or a resolution that the destination refuses, gets 409. Another
body gets 400.

Checking refuses a declaration that binds authorization when its schema does
not define `outbox_destination` with the permission `resolve`, also when the
declaration binds no mixin: the runtime serves these routes in each service on
the runtime data path. Checking also refuses a `resolve` that reaches a
wildcard subject such as `user:*`, and an entity with the resource type
`outbox_destination`.

Give `resolve` to the operators only. Checking cannot see a `resolve` that
reaches each member of a tenant, such as through a relation that every member
holds. Such a member can send a parked message again or discard it. Do not
write `resolve = nil` either: then no operator can resolve a message.

**Warning:** Do not discard a parked message of the destination `authz`
unless you apply its change in another way. The messages of `authz` are the
relationship writes and deletes of SpiceDB. When you discard a message that
removes an access, SpiceDB keeps that access: the revocation is never
applied. Send the message again with `not_acted`. The destination `authz` is
idempotent, so it refuses `acted`.

### Take the change events of another service

A worker can consume the change events of another service through a
subscription. See [Subscriptions](subscriptions.md).

### A reference held within a field

A reference can be held within a field that the row and the referenced row
must have equal. Declare it with `within` on the reference. For a Ticket of a
Project that refers to a Sprint of a Project, the reference is
`sprint_id: {relation: sprint, permission: edit, within: project_id}`.

The field must be a required reference to the same entity on both entities,
or the scope field of a collection over each entity (ST-DECL-INV93). Use
`within: none` when the reference crosses that entity on purpose, such as a
link between issues of two projects. Checking warns about a reference with no
`within` when both entities refer to the same entity.

A create, and an update that changes the reference or the field, compares the
field of the new row with the loaded referenced row before the row statement.
A difference is refused with 400, the code `invalid` and the field of the
reference, and nothing is written (ST-SUP-RP10-LC37). An update that changes
only the field must also give the loaded row of the reference that stays; the
generated CRUD handlers do this. The database holds the same rule with a
composite key (ST-SUP-RC01-OC46), so a race, a worker that gives no parent and
a move of the referenced row are refused too: a move of a sprint to another
project while a ticket of the first project refers to it gives 400 `invalid`
with the field `project_id`.
When the field that the reference is held within is optional, the database
also holds a CHECK: a row that sets the reference and leaves that field empty
gives 400 `invalid` with the field of the reference.

### A failed statement

When a statement of a command, a worker or a consumer fails and the failure is
not a conflict, the runtime writes one log line `command: a statement failed`
at the level error. The line gives the command or worker, the step, the cause
class and the SQLSTATE class. It never gives a value or the database text. The
cause classes are:

| Cause | What it is |
|---|---|
| `data` | A value of the row does not fit, such as a foreign key or a check (SQLSTATE classes 22 and 23). |
| `not_granted` | The role lacks a privilege (42501). |
| `schema` | The statement does not fit the database, such as an undefined column (the other values of class 42). |
| `statement` | Another SQLSTATE. |
| `scan` | The row has other columns than the generated Scan of the entity reads. |
| `other` | An error with no SQLSTATE, such as a lost connection. |

The runtime writes at most one line for each command or worker, cause and
SQLSTATE class in each 10 seconds, with the number of lines that it held back.

From runtime v0.165.0, a statement that the database refuses with SQLSTATE
0A000 `cached plan must not change result type` is not a failed statement.
This occurs when a person drops a column and adds it again while the service
runs: the prepared statement of a connection is then stale. The runtime makes
each connection of the pool prepare its statements again, and runs the
command once more with no wait. A query answers 500 one time, and the next
call passes (ST-SUP-RP04-LC11).

### A value that breaks a declared constraint

Each entity checks the declared constraints of its fields before a create or
an update sends a statement: `min_length`, `max_length`, `pattern`, `min`,
`max` and the values of an enum. An optional field is checked only when it is
set. A value that breaks one is answered 400 with the code `invalid` and the
field, or `InvalidArgument` over gRPC. The reason names the constraint and its
declared bound, never the value.

When the table refuses a value that the check does not cover, the CHECK
(SQLSTATE 23514), the NOT NULL (23502) or the foreign key (23503) of a
declared field gives the same 400 with the field. A unique violation (23505)
stays 409 `conflict`. When the violated index is a declared unique key, a
`unique` field or a `unique_composite`, the answer names the fields of the key
in `field` and never a value: the detail of a generated CRUD write is `the
Organisation was not written: another Organisation has the same name, which
must be unique`, and over gRPC the status `Aborted` carries a `BadRequest`
detail with the fields. Any other conflict keeps the detail `the
<Entity> changed during the request`.

The 409 names the key. Thus a caller who may create or update the entity
learns that a row with that value of the key exists in the tenant, also a row
that the caller may not read. A unique violation told this before too, with no
name of the key. When the existence of a value must not be known across
parents, such as the name of a project in another team, do not make the field
unique in the whole tenant. Make it unique under its parent with
`unique_composite`, which holds the reference to the parent and the field,
such as `{ name: name, type: string, unique_composite: [team_id, name] }`. Then
the conflict tells only of the rows under the parent of the write. A get query of a key that is not a UUID is a missing
row: 404, or `NotFound` over gRPC, with no statement.

### Outage answers

When the authorization provider does not answer in the decision time limit,
2 seconds by default, or the database cannot serve now, a command or a query
answers 503 with `Retry-After`, or `Unavailable` over gRPC. It never waits
for a provider that does not answer. The code of the answer is `undecided` for
the provider and `unavailable` for the database. The database cannot serve
now when it refuses, resets or loses the connection, or gives SQLSTATE class
08, 57P01, 57P02, 57P03 or 53300. Each such step writes one warn line
`command: the database cannot serve now` with the command and the step, at
most one line in each 10 seconds. A COMMIT whose outcome is not known stays
500, because the client must not take it as not done.

### Read your own write over gRPC

A gRPC command whose transaction wrote a tuple answers the header metadata
`authz-revocation: pending` and `authz-revocation-location` with the path
`/authz/revocations/{id}`, as the HTTP headers do. To read the write at once,
send the request metadata `authz-revocation-after` with that path or the
identity, and `prefer: wait=N` when you want a shorter wait. The query or the
stream waits at most N seconds, the wait cap (10 seconds) and half the time
that the deadline of the call leaves. It answers the last state in the header
metadata `authz-revocation`, and `unknown` for a revocation that is not yours.
A bad value is `InvalidArgument`.

### Write a child at once after its parent

A create answers `Authz-Revocation: pending` and `Authz-Revocation-Location`
until the relay writes its tuples to SpiceDB, often for less than one second.
Until then, a write that is decided on the new row, such as the create of a
child under it, gets 404. To create the child at once, send the
`Authz-Revocation-Location` of the parent's create in the header
`Authz-Revocation-After` of the child's create, and `Prefer: wait=N` when you
want a shorter wait:

```bash
curl -X POST https://<host>/api/projects/v1/projects/$PROJECT/tasks \
  -H "Authorization: Bearer $TOKEN" \
  -H "Idempotency-Key: \"$KEY\"" \
  -H "Authz-Revocation-After: $LOCATION" \
  -d '{"title":"first task"}'
```

The write waits for that revocation before it decides, as a read does: at
most N seconds, the wait cap (10 seconds) and half the time that the deadline
leaves. Over gRPC, send the request metadata `authz-revocation-after`. Each
generated CRUD write and each declared command takes it. The answer does not
report the wait: its `Authz-Revocation` names the revocation of its own write.
A revocation that is not yours, or one that is still pending at the end of the
wait, does not fail the write: the write is then decided as with no header. A
value that is not a revocation is 400, or `InvalidArgument` over gRPC. The
browser proxy of a browser-service sends the header to the API as it is.

A Provision that grants admins or workers answers `Authz-Revocation: pending`
with no location. Send `Prefer: wait=N` with the Provision to wait for the
grants. When the answer says `complete`, the first command of an admin is
decided with the grant. A wait that stays pending for the whole wait cap
writes the warn line `authz.revocation.pending_past_cap`: check that the
worker mode, which runs the relay, runs.

### Release notes for runtime v0.180.0

- The runtime writes a counted compare-only value that a read gave only into
  its own field and row, in the attempt that read it. A fill that gives it to
  a compare-only field with no limit, or to another row, gets an internal
  error, and the command commits nothing. Thus a fill cannot get the stored
  hash back through a field with no limit, and cannot give the hash a new
  count of 0 in a new row (ST-SUP-RP10-LC54, security review of PR #479, L1).
  New methods `sealed.Compared.IsCounted` and `sealed.Compared.CounterBy`.
- A rerun of a command in the same request does not charge again the checks
  that an earlier attempt charged only for the same stored value. A new PIN
  that a reset wrote between two attempts gets its own charges (security
  review of PR #479, L2).
- The charge of a check waits at most 1 s for a lock, such as behind the reset
  of a command that has not committed; then the command answers 503. When
  each connection of the pool of the charges is in use, the runtime writes
  the warn line `command.attempts.pool_saturated` with the entity and the
  counts of the pool, and no value, at most once in 10 s (security review of
  PR #479, L3).

### Release notes for runtime v0.178.0

- The charge of a check of a counted compare-only secret runs on a small pool
  of its own, of at most 4 connections, apart from the main pool. Before, it
  took a second connection of the main pool, so a full main pool made the
  check wait for 5 s and the command answered 503. New field
  `command.Config.ChargeDB`; with no value, the charges use the audit pool
  (ST-SUP-RP10-LC54, security review of PR #451, L2).
- A rerun of a command in the same request, such as after a conflict, a stale
  prepared statement or a seal switch, does not charge again the checks that an
  earlier attempt charged (security review of PR #451, I1 of the second
  review).
- `sealed.Compared.Stored` and `StoredContext` give the new error
  `sealed.ErrCounted` for a counted stored value, and no hash. The new
  `sealed.Compared.StoredBy`, `sealed.StoreKey` and `sealed.NewStoreKey` give
  the hash only to the runtime that bound the counter with the new
  `sealed.CountedByKey`. `sealed.CountedBy` binds no store key, and both keep
  the counter of a value that has one (ST-SUP-RP16-LC03, security review of PR
  #451, L1).
- `lease.SealsText` adds the columns and the default of `stego_schema.seals`
  only when they are not there, so a run again takes no `ACCESS EXCLUSIVE` lock
  on the table (security review of PR #472, I2).

### Release notes for runtime v0.177.0

- The cause of `worker.run.failed` and of the log line
  `command.consumer.run_failed` is `fill_json`, `fill_parse` or
  `fill_canceled` for a fill error whose chain holds an `encoding/json` error,
  a `strconv` `NumError` or a `time` `ParseError`, or `context.Canceled`, and
  `fill_error` for another fill error. The runtime reads the type of the
  error, never its text. The three new classes count for the poison bound as
  `fill_error` does (ST-SUP-RC35-OC06, decision ST-SUP-Q203 (b)). New values
  of `probe.WorkerCauses`. See [A failed run of a consuming
  worker](#a-failed-run-of-a-consuming-worker).
- The failure record of a migration Job step that SpiceDB failed gives the
  cause class of the gRPC status code, such as `unavailable`, never the text
  (ST-SUP-RP14-LC03). New method `service.StepError.CauseClass`, new function
  `authz.CodeClass`. See [When the Job fails](delivery.md#when-the-job-fails).

### Release notes for runtime v0.176.0

- A worker can declare `reads: [Entity]`. Its fill gets a read-only
  repository `<Entity>Reader`, of the new type `command.Reader`, with `Get` and
  `List` only, and values `<Entity>View` with no secret field
  (ST-SUP-RP10-LC53, ST-SUP-RC35-OC19, decision ST-SUP-Q186 (a)). New field
  `command.WorkerSpec.Reads`, new field `command.EntitySpec.ReadOnly`, new
  function `command.ReadRepo` and new field
  `service.WorkerParts.ConsumerReadTables`, which the generated code fills. The
  migration job gives the worker role of a consuming worker only `SELECT` on
  the columns that the view reads.
- A compare-only secret with `attempts: {limit: N}` has a limit of wrong
  checks. The runtime charges each check in its own table `attempt_counts`
  before it checks the value, so the count stays after a refusal and parallel
  checks stop at the limit. A charge that cannot commit answers 503
  (ST-SUP-RP10-LC54, decision ST-SUP-Q185 (a)). New `command.CounterSpec`,
  `command.NewCounter`, `command.CountedAt`, `command.HashCountedOf`,
  `idempotency.AttemptsTable`, `sealed.Counter`, `sealed.CountedBy` and
  `sealed.Compared.IsNew`. See [Limit the wrong
  attempts](fills.md#limit-the-wrong-attempts).
- An entity with `events: {fields: [...]}` puts the declared fields of the row,
  as `data`, and their version, as `data_version`, in each change event
  (ST-SUP-RP10-LC55, decision ST-SUP-Q184 (c)). New fields
  `command.EntitySpec.EventData` and `EventVersion`, and the functions
  `command.EventValue` and `command.JSONValue`. See [The fields of a change
  event](fills.md#the-fields-of-a-change-event).

### Release notes for runtime v0.139.0

- A fill that returns `command.ErrNotFound` answers 404 with the code
  `not_found`, the audit outcome `failed`, and `NotFound` over gRPC
  (ST-SUP-RP10-LC51). The answer is the same for a row that is not there and
  for a row that the caller may not read. A fill that maps the error to a
  declared refusal keeps its refusal. New constant `command.ErrorCodeNotFound`.
- Each other 500 of a fill writes one error line `command.fill.failed` with
  the command and a fixed cause class: `fill_error`, `closed`, `internal`,
  `sealed`, `panic` or `deadline`, unless a line of a failed statement or of
  an internal error already names the cause. The line holds no error text. It
  writes at most one line for each command and cause class in ten seconds,
  with `held_back`.
- Provision relates the tenant resource to the service-level resource of the
  tenant when the schema declares the relation `service` on the type `tenant`,
  and Retire deletes it (ST-SUP-RC27-OC18, ST-SUP-RP10-LC52). New field
  `command.TenantSpec.ServiceRelation`, constant
  `command.TenantServiceRelation`, and field `service.Parts.Relations`, which
  the generated code fills.
- Provision and Retire store a keyed fingerprint and a sealed answer in the
  key table when the service has a key ring, as each other command does
  (ST-SUP-RP16-LC05).
- The start of a service refuses a database whose migration ledger holds
  another digest of a migration of the release, at the stage
  `service.start.migration`, with `service.ErrMigrationChanged`
  (ST-SUP-RP14-LC13). New field `service.Parts.Migrations`, which the
  generated code fills from `MigrationDigests` of postgres-adapter 6.15.0.

### Release notes for runtime v0.136.0

- A command waits for the caller's revocation before it decides
  (ST-SUP-RP10-LC47): `command.Serve` and `command.ServeRPC` read
  `Authz-Revocation-After`, or the metadata `authz-revocation-after`, and wait
  as a read does. New methods `command.Runtime.AwaitRevocation` and
  `command.Runtime.AwaitRevocationRPC`, which the generated CRUD writes call
  before `command.Run`. See [Write a child at once after its
  parent](#write-a-child-at-once-after-its-parent).
- A unique violation of a declared key names the fields of the key
  (ST-SUP-RP10-LC48): `command.OutcomeOf` gives the fields in `Field` and the
  reason `command.ReasonUnique`. New field `command.EntitySpec.Unique`, which the
  generated code fills, and function `command.UniqueKey`.

### Release notes for runtime v0.134.0

- A consumer runs no fill before SpiceDB holds the tuple of its own worker in
  the tenant of the message (ST-SUP-RC35-OC18). A message whose tuple is
  missing goes back to pending with the failure code `worker-tuple-missing`
  and the cause `worker_tuple_missing`, and the poison count does not count
  it. Thus a subscription that a release adds no longer parks its backlog
  before the controller of the release writes the tuple. New field
  `command.Config.WorkerTuples`, constants `command.WorkerTupleMissingCode`
  and `command.HeldTupleAge`, and the class `worker_tuple_missing` in
  `probe.WorkerCauses`. A read of the tuple that fails is undecided, with the
  cause `undecided`. A tuple that stays missing for 15 minutes
  (`command.WorkerTupleAlertAfter`) logs the warn line
  `command.consumer.worker_tuple_missing` with the worker, at most once in
  each 15 minutes.
- The key set reads the refresh age of a key document from its
  `Cache-Control` max-age, from `authn.MinAge` (1 minute) to `authn.MaxAge`
  (5 minutes) (ST-SUP-RP13-LC23). From half the refresh age a known key ID
  starts one load beside the request; at the refresh age it waits for the
  load, so a key that the issuer removed stops within the refresh age. New
  function `authn.RefreshAge`.
- A cursor key ring: `query.Config.ExtraCursorKeys` opens cursors and seals
  none, and the api mode reads the new optional variable
  `STEGO_QUERY_CURSOR_EXTRA_KEY_FILE` (ST-SUP-RP11-LC23). Rotate the key in
  three rollouts: give the new key as the extra key; give the new key as the
  key and the old key as the extra key; after the longest list session,
  remove the extra key. No cursor is refused.
- The tenant check does not run for a request that the main router matches
  to no route: such a request gets 404, or 405, and not 403
  `tenant.not_provisioned` (ST-SUP-RC27-OC14).
- Retire stages, for each tenant-qualified type of the schema, the delete of
  the tuples of the rows of the tenant (ST-SUP-RC27-OC15). The relay deletes
  them by resource ID prefix in parts (`authz.TenantDeleteLimit`, 1000, and
  `authz.TenantDeleteCalls`, 10, in one attempt), with no lock of the floor
  row, raises the floor after each attempt, and logs
  `authz.tenant_tuples.deleted` with the type and the count. New operation
  `authz.OperationDeleteTenant`, function `authz.TenantPrefix`, method
  `authz.Handle.UnrelateTenant` and interface `authz.PrefixDeleter`.
- One problem form (ST-SUP-Q171 (a)): the type of each problem of the command
  runtime, the query runtime, the idempotency check, the tenant check and the
  provisioning routes is `<error_type_base><path>`, where the path is the code
  with `.` and `_` as `-`, such as `tenant-not-provisioned`, or the name of the
  status when there is no code, such as `not-found`. With no
  `error_type_base` the type is `about:blank`. The codes do not change. A
  client that reads the type, not the code, must read the new types. New
  function `authn.ProblemTypeOf`, field `query.Config.ProblemBase` and method
  `query.Runtime.WriteProblem`.
- The tenant list of the operator (ST-SUP-Q172 (a)): `GET <base
  path>/stego/tenants`, when the schema declares `permission list_tenants`
  on the resource type of the provisioning. It pages with `limit` and
  `cursor`, answers `items` and `next`, and writes the audit row
  `list-tenants`. New function `command.ServeTenantList`, method
  `service.Service.TenantList`, function `tenant.Entries` and method
  `authz.Handle.DecideService`. Add `permission list_tenants = operator` to
  the definition of the resource type of the provisioning to serve it.
- Upgrade note: a message that an older runtime counted keeps its attempt
  count, and so its back-off, after the upgrade. The runtime does not reset
  it, because the outbox row holds no runtime version, and a new column would
  need a schema step of every service. The back-off has the bound
  `RetryMax` (5 minutes by default), so the next attempt comes at most 5
  minutes after the upgrade. A failure code of an older form starts the
  poison count at 1. When such a message is parked, resolve it with
  `not_acted` to send it again.

### Release notes for runtime v0.133.0

- Each call of the decision provider has a time limit, 2 seconds by default
  (`authz.DefaultDecisionTimeout`, `authz.Config.DecisionTimeout`). A call
  past it is transient: 503 with `Retry-After`, or `Unavailable` over gRPC.
  New cause `authz.CauseStoreUnavailable` for a revocation floor that the
  database cannot read now.
- New function `tenant.Unavailable`. `command.ErrUnavailable` and the code
  `command.ErrorCodeUnavailable` answer a database that cannot serve now with
  503; `query.ErrorCodeUnavailable` does the same for a query, and
  `query.Transient` also reports such an error. `command.RPCCode` maps 503 to
  `Unavailable`.
- `command.EntitySpec` has the new fields `Check` and `Constraints`, and the
  new helpers `command.MinLength`, `MaxLength`, `OneOf` and `MustPattern`.
  `query.Spec` has the new field `ValidID`, with `query.ValidUUID`.
- A worker's create of a row with no parent is decided on the tenant resource
  with the permission of the worker, and in a consumer also as the staging
  principal (ST-SUP-Q164 (b)). New error `command.ErrNoParent` and function
  `command.NoParentEntity` stay for the API; no call gives the error now. The
  class `create_without_parent` is in `probe.WorkerCauses`.
- `command.ServeRPC` sends the revocation metadata of a write.
  `query.Runtime.AwaitWriteRPC` waits for the revocation of the request
  metadata, which `query.ServeRPC` and `query.ServeStream` call;
  `query.StreamRPC` has the new field `Runtime`. New constants
  `authz.RevocationMetadata`, `RevocationLocationMetadata`,
  `RevocationAfterMetadata` and `PreferMetadata`.
- `command.ServeTenant` answers `Authz-Revocation: pending` for a Provision
  that staged a tuple, and waits with `Prefer: wait=N`.
- `authz.Config.PrincipalWaiters` (default `authz.DefaultPrincipalWaiters`, 4)
  limits the waits of one principal. A wait that the cap ends while the
  revocation is pending logs `authz.revocation.pending_past_cap`.
- The status route and the generated wait code give no internal code when the
  client goes away during a wait.
- New function `service.ReadModeSettings`: only the api mode reads
  `STEGO_QUERY_CURSOR_KEY_FILE`.
- The failure class of the Kafka inbox names the operation that failed:
  `broker:<CODE> (describe-configs)`, `(fetch)` or `(commit)`. New function
  `kafkainbox.ValidateMigration` and the step `kafka_inbox_validate`, which
  validates the CHECK of the refused reasons in a transaction of its own.

### Release notes for runtime v0.132.0

- New function `outbox.NewDeclaredResolver` takes the declared rules of each
  destination (`outbox.Rules`); `outbox.NewResolver` is deprecated. A resolution takes the stricter of the row and the
  declared rules, and a message of a destination that the release does not
  declare answers 409. `service.Parts` has the new field `Destinations`, which
  generated code sets. New functions `authz.Rules`, `auditoutbox.Rules` and
  `kafkaoutbox.Rules`.
- The Kafka inbox reads the tenant again in the transaction of each record,
  under the lock of the tenant list (new function `tenant.HoldList`), so a
  record of a tenant that a Retire retired after the lookup is refused as
  `retired-tenant`. `kafkainbox.Config` and `service.InboxEnv` have the new
  field `TenantSchema`. The step `kafka_inbox` grants the worker role SELECT
  on the tenant list.
- The console of the log provider keeps a rate-limit budget of its own for
  the lines of severity ERROR and FATAL. The probe console holds the built-in
  worker events, such as `worker.run.failed`, to the same rate limit.
- `authz.RelayConfig` has the new field `Relations`, the allow list of the
  relations that the relay writes; `NewRelay` refuses no list. Generated code
  sets it from the authorization schema.
- An attempt of the Kafka destination that no broker answers logs
  `kafka.delivery_failed` with the class `broker-unavailable`. After 5 such
  attempts in a row, `/readyz/modes` of the worker mode answers 503 with
  `the destination kafka fails: broker-unavailable`. `outbox.Destination` has
  the new field `Failing`, and `outbox.Worker` the new function `Failing`.
- A Retire deletes the tuples of the admin relation of the tenant.
- The migration of the refused records of the Kafka inbox replaces the CHECK
  of the reasons when the list of reasons changes.
- New function `pgtest.ReapNetwork` (test support).

### Release notes for runtime v0.131.0

- The log provider of the telemetry runtime writes each record at the level
  warn or higher as one JSON line on standard output, with or without an
  OTLP endpoint (ST-SUP-RP03-LC16). `otel.Runtime.LoggerProvider` is no longer
  nil without a collector. New functions `ConsoleDropped` and
  `ConsoleFailures`.
- New parked-message routes of the api mode (ST-SUP-RP14-LC09,
  ST-SUP-RP14-LC10): `service.ParkedRoutes`, `Service.RegisterParked`,
  `outbox.Resolver`, `outbox.Queue.Parked`, `outbox.ErrRefusedOutcome`,
  `authz.Runtime.StateHook`, `query.KeyCursor` and `query.Cursor.Key`.
- New function `command.Snapshot`: the generated contract package takes a
  copy of each declared list when it starts, and the handle refuses a list
  that a fill changed after that.
- `command.ListValue` has the new field `KeyOf` (`command.KeyUUID`), and `command.Reference`
  the new field `Check`. Generated code sets them; code that does not set them
  needs no change.
- The Kafka inbox reads the retention and the cleanup policy of its topic
  again each minute, refuses a topic with no cleanup policy and a record whose
  key names another row than its payload.
- A worker can create a row that has a reference: the authorization
  migration gives the worker role the same grants as the runtime role on the
  revocation floor and records. A refused statement of the authorization
  handle keeps its SQLSTATE, so a missing grant parks a consumed message
  (cause `not_granted`) and does not retry it without end.
- A provision of a retired tenant ID answers 409 with the code
  `tenant.retired`; a provision of an active ID keeps `tenant.provisioned`.
  New error `tenant.ErrRetired` and code `command.ErrorCodeRetired`.
- The problem types `idempotency-key-missing`, `idempotency-key-in-progress`
  and `idempotency-key-reused` of a command sit under the `error_type_base`
  of the service. `command.Config` has the new field `ProblemBase`.
- The readiness of the worker mode names the stage of a stopped Kafka inbox,
  such as `the component inbox-order-changes stopped at the stage
  kafka-consumer.retention`.

### Release notes for runtime v0.130.0

- New package `outbox/kafkainbox`: the Kafka inbox of a subscription
  (ST-SUP-RP01-LC35 to ST-SUP-RP01-LC38). New functions
  `outbox.Queue.Receive`, `outbox.Worker.AddPurge`, `outbox.Worker.Retention`,
  `kafkaoutbox.Connection` and `kafkaoutbox.ReadConsumer`. `service.WorkerParts` has the new field
  `Inbox`, and `service.Migration` has the new field `Inbox` with the step
  `kafka_inbox`. Code that does not set them needs no change.

### Release notes for runtime v0.127.0

- A command fill gives no SQL text. Before v0.127.0, a command fill that read
  a set of rows gave its own SQL text in `command.ListSpec` and made its list
  with `command.NewList`. That SQL named the columns of the entity, so the
  first schema change broke it with 500 `internal` (walkthrough 7, B1). Now
  every handle takes only the lists that its declaration declares
  (ST-SUP-RP10-LC38). The fill still compiles, but each call that uses its own
  list fails with this error:

  ```text
  command: internal error: the command place-order reads the list of-order of
  Line, which its declaration does not declare: since runtime v0.127.0 a fill
  gives no SQL text, so declare the list under lists: of the command and use
  the generated list of the command contract (ST-SUP-RP10-LC38)
  ```

  To move a fill, write the `WHERE` terms of its `Read` statement as `filter`
  terms under `lists:` of the command. Write a term whose value came from the
  fill as `param:`. Run `stego apply`. Then remove the `command.ListSpec` and
  the `command.NewList` call from the fill, and call the generated
  `<Command><List>With` function in place of the filter values.
- A list term can give a parameter (`param:`), for a command and for a worker.
- A list of a child entity takes its decided parent row (`parent: true`), and
  a parameter of a reference is checked with the key check of the entity.
- The generated upsert of a collection finds its row through a generated list.
- A failed statement writes one log line with its cause class and SQLSTATE
  class (see "A failed statement").
### Release notes for runtime v0.126.0

- A reference can be held within a field (ST-SUP-RP10-LC37). `Reference` has
  the new fields `Field`, `Within` and `Key`, and `EntitySpec` has `Within`
  and `Referrers`. Generated code sets them; code that builds an `EntitySpec`
  by hand needs no change while no reference declares `within`.
- A foreign key violation of a composite key of `within` is `ErrInvalid` that
  names the reference, not an internal error.

### Release notes for runtime v0.125.0

- A worker fill gives no SQL text. Before v0.125.0, a worker fill gave its
  own SQL text in `command.ListSpec` and made its list with `command.NewList`.
  That SQL named the columns of the entity, so each added column broke it.
  Now the handle of a worker takes only the lists that its declaration
  declares (ST-SUP-RP10-LC38). The fill still compiles, but each run that
  uses its own list rolls back with this error:

  ```text
  command: internal error: the worker low-stock-scan reads the list low of
  StockItem, which its declaration does not declare: since runtime v0.125.0 a
  worker fill gives no SQL text, so declare the list under lists: of the
  worker and use the generated list of the worker contract (ST-SUP-RP10-LC38)
  ```

  To move a fill, write the `WHERE` terms of its `Read` statement as `filter`
  terms under `lists:` of the worker, and add `lock: true` when the fill used
  `Lock`. Run `stego apply`. Then remove the `command.ListSpec` and the
  `command.NewList` call from the fill, use the generated list, and remove the
  filter values from the `List` and `ListForUpdate` calls.
- The outbox worker gives each destination claim slots of its own, so a slow
  or refused destination does not delay another destination. It runs
  `Concurrency` slots, or one slot for each destination when there are more
  destinations. Each slot holds one database connection, so
  `outbox.Worker.Run` refuses a pool that holds fewer connections than the
  slots plus 2. The worker mode of a generated service sets its pool to this
  size. A program that builds its own outbox worker must give its pool at least
  that many connections.

## Declare an observation

An observation is the only correct path for a fill to emit a log line or a
domain signal.

<!-- excerpt -->
```yaml
observations:
  - name: low_stock
    kind: event
    severity: warn
    fields:
      - {name: level, type: "enum(low | empty)"}
      - {name: free, type: "measure(1, 0..1000000)"}
    emitted_by: [low-stock-scan]
```

| Key | What it is |
|---|---|
| `name` | The name, in lower snake case. Each signal has the name `domain.<name>`. |
| `kind` | `event`, `counter` or `histogram`. |
| `severity` | `debug`, `info`, `warn` or `error`. |
| `fields` | Up to eight fields, each with a name in lower snake case and a type. Optional. |
| `value` | For a histogram only: the number that each emission records, as `name: unit`, such as `wait: ms`. |
| `emitted_by` | The commands, queries and workers whose fills can emit it, and the slot bindings as `collection.slot`. |
| `counts` | `attempt` or `committed`. The default is `attempt`. |

The type of a field is one of these:

- `enum(a | b)`: one to 32 values in lower snake case.
- `bool`.
- `measure(<unit>, <min>..<max>)`: a whole number in a range. The unit is
  `s`, `ms`, `By` or `1`.
- `Entity.field`: a field of a declared entity. At this time a worker cannot
  use this type: validate refuses it with `the entity StockItem needs exactly
  one Go model provider, and has 0`.

Validate refuses free text, a plain `int` and all other types.

The probe of each operation in `emitted_by` has one method for each
observation. The fill calls it through the handle:

```go
h.Probe().LowStock("low", free)
```

The arguments are the fields, in the order of the declaration. A fill imports
only its slot contract, so it cannot import the package `out/probes`, which
holds the enum type `LowStockLevel`. Give an enum value as a string constant,
such as `"low"` or `"empty"`.

## The fill workflow

Do these steps for each command, query and worker. The example is the command
`place-order`. For a query, give the slot `query.query`. For a worker, give
the slot `worker.worker`.

1. Declare the command in `service.yaml`, and add its messages to the proto
   file. For a worker, add the schema relations. Then run:

   ```bash
   stego validate
   ```

   Validate names each fill that is missing and its slot, such as `write the
   fill of the slot command.command there`.

2. Make the fill. You can do this before the first apply, because the
   contract need not exist yet:

   ```bash
   stego fill create place-order --slot command.command
   ```

   The output is:

   ```text
   Created fill "place-order" of the command place-order for slot "command.command"
     fills/place-order/fill.yaml
     fills/place-order/fill.go
     fills/place-order/fill_test.go
   Run stego apply to write the contract of the fill, and stego deps to resolve its modules; then write the fill.
   Until it is written, the fill returns ErrNotImplemented.
   ```

   The new fill exports `New`, or `Fill` for a query, and returns
   `ErrNotImplemented`. Its test passes while the fill refuses each call. When
   you write the fill, the test fails and tells you to replace it with the
   tests of what the fill does. Fill create refuses a fill that no command, query or worker
   of `service.yaml` names, and a fill that exists.

3. Write the fill in `fills/place-order/fill.go`, and write its tests. Then
   test the fill and sign its evidence with your own key:

   ```bash
   stego test
   stego fill sign place-order
   stego fill status
   ```

   `stego test` tests the fill against the contract that apply plans to write,
   so you can do this before the first apply. The project's files do not
   change. When a fill records no evidence, `stego test` names it and exits
   non-zero. `stego fill status` shows the fill as `qualified`.

4. Generate the contract, resolve the dependencies and build:

   ```bash
   stego apply
   stego deps
   go build -o service ./out
   ```

   Plan and apply refuse a fill of a command, query or worker that is not
   qualified, and name it and what to run. `stego validate` gives only a
   warning for a fill that has no evidence or no sign-off, such as
   `fill "place-order" is untested ...; run stego test, then stego fill sign
   place-order; plan, apply and build refuse the fill until then`. A fill folder that holds only
   `fill.yaml` does not stop apply: apply writes the contract, then names the
   fill to write.

The file `fill.yaml` names the fill and its slot, and no collection: the
command, query or worker that names the fill is its binding.

```yaml
kind: fill
name: place-order
implements: command.command
```

The slot of a command fill is `command.command`, of a query fill
`query.query`, and of a worker fill `worker.worker`.

### Qualification of these fills

These fills ship in the service with write access to the entities in
`writes`, so they are qualified as each slot fill is (decision ST-EXT-Q30).
The contract digest of such a fill covers the part of the generated package
(`out/commands`, `out/queries` or `out/workers`) that the fill compiles
against: the declarations of its own command, query or worker, and the
declarations that the fill uses. A change to the fill, or to that part, makes
the fill stale: run `stego test`, then `stego fill sign <name>` again. A new or
changed command does not make the other command fills stale, unless it changes
a declaration that they use.

`stego deps` and `stego build` refuse such a fill that is not qualified, as
plan and apply do. `stego build` also compares the contract digest of the
evidence with the contract that the source revision holds. `stego test` passes
only `-v`, `-race`, `-count` (1 or more), `-timeout` and `-failfast` to
`go test`, and records evidence only when at least one test of the fill ran
and passed.

Upgrade: a project whose fills of commands, queries or workers have no
signed evidence is refused at plan. Run `stego test`, then
`stego fill sign <name>` for each fill that plan names. `stego fill sign` keeps a
sign-off that already covers the current evidence with a key that `SIGNERS`
lists, and writes nothing; give `--force` to sign it again with your key.
