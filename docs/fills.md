# Fills: the developer reference

This page is the reference for the developer who writes the fills of
commands, queries and workers. It gives the field settings, the secret
fields, the methods of a handle and the decision that each method makes, the
change events, and how to test a fill.

Read [Commands, queries and workers](commands-queries-workers.md) first. That
page tells you how to declare a command, a query and a worker, and how to
make, test and sign a fill.

## The example service

The example is the service `wallets`. A customer has wallets, and each wallet
has transactions. Each wallet has a readable secret `api_key` and a
compare-only secret `pin`, with a limit of wrong PINs. The commands take
money from a wallet, move money between two wallets, change the PIN, and start
a campaign of the tenant. The worker `sum-balances` reads the wallets and
writes the daily total of the balances every hour. The change events of
customers, wallets and transactions carry declared fields.

A test of the STEGO repository runs `stego validate` on this declaration, with
the schema and the proto file below (ST-STD-DOCS-R02). Each smaller part of a
declaration on this page is a part of this declaration. The Go code on this
page compiles against the code that `stego apply` writes for it.

```yaml
kind: service
name: wallets
archetype: rest-crud
language: go
base_path: /api/wallets/v1
error_type_base: https://errors.example.test/wallets/

tenancy:
  kind: pooled
  key: tenant_id
  claim: tenant
  provisioning: {resource_type: service, provision: provision, retire: retire, admin: admin}
authorization:
  provider: spicedb
  schema_path: authz/schema.zed
  subject_from_identity: user
audit:
  level: change

entities:
  - name: Customer
    resource_type: customer
    permissions: {create: public, read: view, list: view, update: edit, delete: edit}
    public_reasons: {create: a new customer has no resource to decide on before it exists}
    relationships:
      - {relation: owner, subject_source: caller, subject_type: user}
    fields:
      - { name: name, type: string, min_length: 1, max_length: 120 }
      - { name: email, type: string, max_length: 255, sensitivity: pii }
    events:
      fields: [name, email]
      pii_reasons: {email: the ledger sends each customer a receipt}
  - name: Wallet
    resource_type: wallet
    permissions: {create: public, read: view, list: view, delete: edit}
    public_reasons: {create: a new wallet is decided on edit of its customer through the reference customer_id}
    references:
      customer_id: {relation: customer, permission: edit}
    fields:
      - { name: customer_id, type: ref, to: Customer }
      - { name: label, type: string, min_length: 1, max_length: 80 }
      - { name: balance, type: int64, min: 0, default: 0, writable: never }
      - { name: api_key, type: string, min_length: 16, max_length: 128, sensitivity: secret }
      - { name: pin, type: string, min_length: 4, max_length: 12, sensitivity: secret, compare_only: true, writable: create, attempts: {limit: 5} }
    events:
      fields: [customer_id, label, balance]
  - name: Transaction
    resource_type: wallet_transaction
    permissions: {read: view, list: view}
    references:
      wallet_id: {relation: wallet, permission: view}
    fields:
      - { name: wallet_id, type: ref, to: Wallet }
      - { name: kind, type: enum, values: [deposit, withdrawal, transfer_in, transfer_out] }
      - { name: amount, type: int64, min: 1 }
    events:
      fields: [wallet_id, kind, amount]
  - name: Campaign
    resource_type: campaign
    permissions: {read: view, list: view}
    fields:
      - { name: title, type: string, min_length: 1, max_length: 80 }
  - name: DailyTotal
    resource_type: daily_total
    permissions: {read: view, list: view}
    fields:
      - { name: wallets, type: int32, min: 0 }
      - { name: balance, type: int64, min: 0 }

collections:
  customers:
    entity: Customer
    operations: [create, read, update, list]
  customer-wallets:
    entity: Wallet
    scope: { customer_id: Customer }
    operations: [create, read, list]
  wallet-transactions:
    entity: Transaction
    scope: { wallet_id: Wallet }
    operations: [read, list]

commands:
  - name: withdraw
    writes: [Wallet, Transaction]
    resource_type: wallet
    permission: operate
    resource_member: wallet_id
    fill: withdraw
    route: POST /wallets/{wallet_id}/withdrawals
    refusals:
      - {name: invalid_amount, code: amount.invalid, status: 422}
      - {name: wrong_pin, code: pin.wrong, status: 403}
      - {name: short_balance, code: balance.short, status: 409}
  - name: transfer
    writes: [Wallet, Transaction]
    resource_type: wallet
    permission: operate
    resource_member: from_wallet_id
    fill: transfer
    route: POST /wallets/{from_wallet_id}/transfers
    refusals:
      - {name: no_target, code: transfer.no_target, status: 422}
      - {name: short_balance, code: balance.short, status: 409}
  - name: change-pin
    writes: [Wallet]
    resource_type: wallet
    permission: edit
    resource_member: wallet_id
    fill: change-pin
    route: POST /wallets/{wallet_id}/pin:change
    refusals:
      - {name: wrong_pin, code: pin.wrong, status: 403}
  - name: start-campaign
    writes: [Campaign]
    resource_type: service
    permission: start_campaign
    fill: start-campaign
    route: POST /campaigns

queries:
  - name: daily-totals
    reads: [DailyTotal]
    resource_type: daily_total
    permission: view
    fill: daily-totals
    route: GET /daily-totals

workers:
  - name: sum-balances
    trigger: every 1h
    reads: [Wallet]
    writes: [DailyTotal]
    resource_type: daily_total
    permission: record
    fill: sum-balances
    lists:
      - name: funded
        entity: Wallet
        filter:
          - {field: balance, op: greater_than, value: 0}

experimental_acceptances:
  - {component: worker, accepted_until: 2027-03-31, reason: the daily totals of the balances}

mixins:
  - durable-events
  - kafka-events

overrides:
  postgres-adapter: { schema_generation: tables-v1 }
  command: { proto_files: [{path: proto/wallets.proto, import_path: wallets.proto}] }
```

The SpiceDB schema is at `authz/schema.zed`:

```zed
definition user {}

definition service {
	relation operator: user
	relation admin: user
	permission provision = operator
	permission retire = operator
	permission start_campaign = admin
}

definition customer {
	relation owner: user
	permission view = owner
	permission edit = owner
}

definition wallet {
	relation customer: customer
	relation tenant: tenant
	permission view = customer->view
	permission edit = customer->edit
	permission operate = customer->edit
	permission record = tenant->worker_sum_balances
}

definition wallet_transaction {
	relation wallet: wallet
	permission view = wallet->view
}

definition campaign {
	relation tenant: tenant
	permission view = tenant->audit
}

definition daily_total {
	relation tenant: tenant
	permission view = tenant->audit
	permission record = tenant->worker_sum_balances
}

definition service_account {}

definition tenant {
	relation service: service
	relation auditor: user
	relation worker_sum_balances: service_account
	permission audit = auditor + service->admin
	permission record = worker_sum_balances
}

definition outbox_destination {
	relation operator: user
	permission resolve = operator
}
```

The messages of the commands are at `proto/wallets.proto`:

```proto
syntax = "proto3";

package wallets.v1;

// WithdrawRequest takes an amount from a wallet. The PIN of the wallet must match.
message WithdrawRequest {
  string wallet_id = 1;
  int64 amount = 2;
  string pin = 3;
}

message WithdrawResponse {
  string wallet_id = 1;
  int64 balance = 2;
}

// TransferRequest moves an amount from one wallet to another.
message TransferRequest {
  string from_wallet_id = 1;
  string to_wallet_id = 2;
  int64 amount = 3;
}

message TransferResponse {
  int64 from_balance = 1;
  int64 to_balance = 2;
}

// ChangePinRequest sets a new PIN. The old PIN must match.
message ChangePinRequest {
  string wallet_id = 1;
  string old_pin = 2;
  string new_pin = 3;
}

message ChangePinResponse {
  string wallet_id = 1;
}

// StartCampaignRequest starts a campaign of the tenant.
message StartCampaignRequest {
  string title = 1;
}

message StartCampaignResponse {
  string campaign_id = 1;
}
```

## The settings of a field

Each field of an entity is one item of `fields`:

<!-- excerpt -->
```yaml
      - { name: api_key, type: string, min_length: 16, max_length: 128, sensitivity: secret }
```

| Key | What it is |
|---|---|
| `name` | The name, in lower snake case. |
| `type` | `string`, `int32`, `int64`, `float`, `double`, `bool`, `bytes`, `timestamp`, `enum`, `ref`, `jsonb` or `money`. |
| `optional` | `true` when a row can have no value. The default is `false`. |
| `default` | The value that a create stores when the request gives none. |
| `min_length`, `max_length`, `pattern` | The bounds of a string. Each write checks them, and the answer of a broken bound is 400 `invalid`. |
| `min`, `max` | The bounds of a number. |
| `values` | The values of an `enum`. |
| `to` | The entity that a `ref` refers to. |
| `currency` | The ISO 4217 code of a `money` field. |
| `unique` | `true` when no two rows of the tenant have the same value. |
| `unique_composite` | The fields of a unique key with more than one field, such as `[team_id, name]`. |
| `client_set` | `true` for a `timestamp` that the client sets, such as a due date. The server sets each other timestamp. |
| `sensitivity` | `public`, `pii` or `secret`. The default is `public`: `sensitivity: public` and no `sensitivity` are the same, and a list searches and orders the field. See below. |
| `compare_only` | `true` for a `secret` that the service only checks and never reads back, such as a PIN. See below. |
| `writable` | `always`, `create` or `never`: which collection writes can set the field. The default is `always`. See below. |
| `attempts` | For a compare-only secret only: `{limit: <1 to 100>}`, the limit of wrong checks. See [Limit the wrong attempts](#limit-the-wrong-attempts). |

An entity can also declare `events`, the fields that its change events carry.
See [The fields of a change event](#the-fields-of-a-change-event).

### sensitivity

- `public`: an ordinary value.
- `pii`: a personal value, such as an email. Answers hold it. The change
  events never hold it, and the audit rows redact it, unless the declaration
  sets `audit.pii` to `keep`. The runtime writes an audit row for each row of an
  entity with a `pii` field that a fill reads. The idempotency store keeps
  the answers of the commands, and thus such a value, for 24 hours by
  default. Since runtime v0.138.0 it seals each stored answer of a service
  with a `pii` field with the key ring (see
  [Idempotency](commands-queries-workers.md#idempotency)).
- `secret`: a value that no answer, audit row, change event, log or query
  holds. The database stores it sealed. A secret is a `string` or a `jsonb`.

A `secret` field cannot be `unique`, in a `unique_composite`, in an upsert key,
`computed`, the subject of a relationship, or the field that a reference is
held `within`. It cannot have a `default` or an implicit value. A schema change
cannot rename it, retype it or give it a fill value. `stego validate` refuses
each of these: a key on a secret tells which rows hold the same value, and a
default puts the secret in clear in the declaration.

### compare_only

A `secret` field is readable, or it is compare-only:

| | Readable secret (`api_key`) | Compare-only secret (`pin`, `compare_only: true`) |
|---|---|---|
| Stored as | AES-256-GCM, sealed with the key ring of the service | An Argon2id hash |
| Go type in a command or a worker | `sealed.Secret` | `sealed.Compared` |
| A fill can get the text | Yes, with `Reveal` | No. A fill can only check a value with `Matches`. |
| Type | `string` or `jsonb` | `string` only |

A service with a `secret` field, readable or compare-only, or a `pii` field
needs the key ring of its secret fields, in the Secret
`<service>-secret-key-ring`. The api, worker and controller modes do not start
without it (runtime v0.138.0). The runtime keys the fingerprints of the stored
answers with the ring and seals the answers, so a reader of the database
cannot try each PIN against a stored request or read a stored `pii` value. See
[Day-2 operations](operations.md) for the key ring, its backup and its
rotation.

A readable secret that the base release stores in clear, such as a field that
was `pii` before, or a secret field of a release before runtime v0.135.0,
moves to its sealed column with a seal step over two releases: see [Seal
steps](schema-changes.md#seal-steps).

### writable

`writable` tells which collection writes can set the field:

- `always`: a create and each update can set it.
- `create`: only a create sets it. An update, an upsert of a stored row and a
  patch refuse another value with 400.
- `never`: no collection write sets it. A create stores its `default`, so the
  field must be `optional` or have a `default`.

A command and a worker can change the field in each case. Thus `balance:
writable: never` lets only the commands of the example change a balance.

<!-- excerpt -->
```yaml
      - { name: balance, type: int64, min: 0, default: 0, writable: never }
```

## Secret fields

### Who reads a secret

| Who | Readable secret | Compare-only secret |
|---|---|---|
| A collection create, update, upsert and patch | Takes it in the body. A write that leaves it out, or gives it empty, keeps the stored value. | Takes it in the body and stores its hash. |
| A collection answer, a list, a query, an event, the audit | Never holds it. The OpenAPI document marks it `writeOnly`. | Never holds it. |
| A command fill, or a worker fill of an entity in `writes` | Reads it with `Reveal`, sets it with `sealed.SecretOf`. | Checks a value with `Matches`, sets it with `sealed.ComparedOf`. |
| A worker fill of an entity in `reads` | Never: the value has no secret field. | Never. |
| A query fill | Never: the value of a query has no secret field. | Never. |

No client can read a secret back. Only the fills of commands and workers can
read a readable secret.

A worker that only reads an entity declares it in `reads`, not in `writes`
(runtime v0.176.0). The worker `sum-balances` of the example reads the wallets
this way. Its fill gets `workers.WalletReader(h)`, a read-only repository with
`Get` and `List` only, whose value `workers.WalletView` has each field of the
wallet except `api_key` and `pin`. The statements of the reader do not read
the secret columns, so a fault in the fill cannot leak or change a secret. A
fill that tries to read `w.Value().ApiKey` does not compile. An entity is in
`writes` or in `reads`, not in both. Give a worker `writes` only for the
entities that it changes.

Put a secret in the response message of a command only when the command
issues it, such as an API key that the client gets one time, and only in a
member that the proto file marks `debug_redact` (see [A secret
member](commands-queries-workers.md#a-secret-member)). Never put a secret that
the service stores, such as a secret field of an entity, in a response. The
idempotency store keeps each answer of a command for the retention of its key,
24 hours by default. With the mark, the runtime seals each stored answer with
the key ring (see [Idempotency](commands-queries-workers.md#idempotency)), and
the generated CLI writes the answer only to `--output-file`. A retry with the
same key gets the same secret again, from the sealed answer. Without the mark,
the answer goes to stdout, and a service with no `secret` or `pii` field keeps
it in clear.

A field with `writable: create` or `never` that is a secret is never compared:
an update that gives it, also with the stored value, gets 400. An answer never
holds a secret: since rest-api 4.14.0 it leaves out the member of each secret
field. An older rest-api gives a required secret as an empty member, such as
`"api_key": ""`. A client that sends back a row that it read must leave out
each secret member.

### sealed.Secret

`sealed.Secret` is the Go type of a readable secret in `out/commands` and
`out/workers`. The package is `github.com/jsell-rh/stego/registry/runtime/sealed`.

| Function or method | What it does |
|---|---|
| `sealed.SecretOf(text)` | Gives a Secret of the text. An empty text gives a Secret that is not set. |
| `s.Reveal()` | Gives the text, or empty when it is not set. |
| `s.IsSet()` | Tells whether the Secret has a text. |
| `s.Equal(text)` | Tells, in constant time, whether the text is the secret. A Secret that is not set is equal to no text. |
| `fmt`, `String`, `slog` | Give `[secret]`, never the text. |
| `json.Marshal` | Gives `null`. A JSON decode into a Secret fails. |

### sealed.Compared

`sealed.Compared` is the Go type of a compare-only secret. A read gives the
stored hash. It never gives the text.

| Function or method | What it does |
|---|---|
| `sealed.ComparedOf(text)` | Gives a new value. The next write stores its Argon2id hash. |
| `c.Matches(value)` | Tells whether the value is the secret. It compares in constant time. An empty value and a Compared that is not set match nothing. |
| `c.IsSet()` | Tells whether the Compared has a new text or a stored hash. |
| `fmt`, `String`, `slog` | Give `[secret]`. |
| `json.Marshal` | Gives `null`. A JSON decode into a Compared fails. |

Each check runs the Argon2id hash, with 19 MiB of memory. The runtime runs at
most as many hashes at one time as the process has CPUs. Each other check
waits.

### Check a compare-only secret

The fill of `withdraw` checks the PIN of the request against the stored hash.
A wrong PIN is a declared refusal. A proto request has no bounds, so the
amount can be 0 or below: the fill checks it first, before the PIN and the
balance. Without that check, an amount of -1000 would add 1000 to the balance.

```go
// Withdraw is the business rule of a withdrawal: the new balance, or a declared refusal. It reads no handle, so a
// plain unit test proves it.
func Withdraw(pinMatches bool, balance, amount int64) (int64, error) {
	if amount < 1 {
		return 0, commands.WithdrawInvalidAmount
	}
	if !pinMatches {
		return 0, commands.WithdrawWrongPin
	}
	if amount > balance {
		return 0, commands.WithdrawShortBalance
	}
	return balance - amount, nil
}

// New gives the fill of the command withdraw.
func New() commands.WithdrawFill {
	return func(ctx context.Context, req *commands.WithdrawRequest, h *commands.WithdrawHandle) (*commands.WithdrawResponse, error) {
		wallets, err := commands.WalletRepository(h)
		if err != nil {
			return nil, err
		}
		wallet, err := wallets.GetForUpdate(ctx, req.WalletId)
		if err != nil {
			return nil, err
		}
		v := wallet.Value()
		balance, err := Withdraw(v.Pin.Matches(req.Pin), v.Balance, req.Amount)
		if err != nil {
			return nil, err
		}
		v.Balance = balance
		updated, err := wallets.Update(ctx, wallet, v)
		if err != nil {
			return nil, err
		}
		transactions, err := commands.TransactionRepository(h)
		if err != nil {
			return nil, err
		}
		_, err = transactions.Create(ctx, commands.Transaction{WalletID: updated.ID(), Kind: "withdrawal", Amount: req.Amount}, updated)
		if err != nil {
			return nil, err
		}
		return &commands.WithdrawResponse{WalletId: updated.ID(), Balance: balance}, nil
	}
}
```

The fill reads the wallet with `GetForUpdate`, so no other command changes the
balance before this transaction ends. The check runs before the balance check,
so a wrong PIN tells nothing of the balance.

### Limit the wrong attempts

A PIN of 4 digits has only 10,000 values. A caller who may operate the wallet,
such as with a stolen session, could try each value in some minutes. Thus the
example declares a limit of wrong PINs on the field (runtime v0.176.0):

<!-- excerpt -->
```yaml
      - { name: pin, type: string, min_length: 4, max_length: 12, sensitivity: secret, compare_only: true, writable: create, attempts: {limit: 5} }
```

Only a compare-only secret can have `attempts`. The limit is from 1 to 100, as
NIST SP 800-63B (5.2.2) asks for each account. `stego validate` refuses each
other limit.

The fill does not change. The runtime binds each PIN that a read of the handle
gives to the count of its row:

- Before each `Matches` checks the value, the runtime adds one to the count, in
  a short transaction of its own that commits before the check. Thus the count
  stays after the fill returns its refusal, such as `pin.wrong`, and the
  runtime rolls back the command. Commands that check one row at the same time
  each see the counts before them: at most the limit of checks run.
- At the limit, `Matches` gives `false` and does not check the value, also for
  the right PIN. The answer is the same refusal, and the check takes the same
  time, so the caller does not learn that the limit is reached.
- When the runtime cannot write the count, such as when the database cannot
  serve, `Matches` gives `false` and the command answers 503 and commits
  nothing. The count has a small pool of its own, of at most 4 connections,
  so a fill that holds the last connection of the main pool does not make its
  check wait (runtime v0.178.0).
- When the runtime runs the command again in the same request, such as after a
  conflict, the checks that an earlier attempt counted do not count again
  (runtime v0.178.0). This is true only for the same stored PIN: when a new
  PIN comes between two attempts, its checks count (runtime v0.180.0).
- The PIN that a read gives does not give its stored hash to the fill:
  `Stored` gives the error `sealed.ErrCounted` (runtime v0.178.0). Check a
  value only with `Matches`.
- The runtime writes the PIN that a read gave only into its own field and row.
  An update of the wallet that keeps its PIN is correct. A fill that gives the
  PIN to a compare-only field with no limit, or to another row, such as a new
  wallet, gets an internal error, and the command commits nothing (runtime
  v0.180.0). To give a row a PIN, give it a new value with `sealed.ComparedOf`.
- A count waits at most 1 s for a lock, such as behind a command that resets
  the count and has not committed. Then `Matches` gives `false` and the command
  answers 503 (runtime v0.180.0). When each connection of the small pool of
  the counts is in use, the runtime writes the warn line
  `command.attempts.pool_saturated`, with no value.
- A `Matches` that matches in a command that commits sets the count to 0. A
  write of a new value of the field, such as a new PIN, also sets it to 0.
- In a worker, `Matches` gives `false` and counts nothing.
- The count is not a field of the entity: the runtime keeps it in a table of
  its own, `attempt_counts`, which the migration job makes. A count never
  changes the row, its version, an audit row or a change event.

The runtime keeps the stored hash of a counted PIN from the fill with a store
key: a Go value of the runtime package that fill code cannot name. The store
key stops a fill that uses the types of the runtime in a way that they do not
allow, such as `Stored` on a counted PIN or a copy of the PIN to another field.
It does not stop code that reads the memory of the process past the Go types:
`reflect`, `unsafe` and the directive `//go:linkname` can read the hash and the
store key. The fill rules refuse these in each fill and in each fill module
(see [The imports of a fill](#the-imports-of-a-fill)), so `stego validate` and
`stego test` refuse a fill that uses them. The store key does not protect a
service whose fill code goes past these checks. It is not a cryptographic key,
and a review of the fill code is still necessary.

Read the row through the handle, such as with `GetForUpdate`, before you call
`Matches`: the read decides the permission of the caller on the row, so a
caller who may not operate the wallet cannot use its checks. Then check the PIN
before any other rule, as `Withdraw` does.

To unlock a wallet, write a command that sets a new PIN, such as with a
permission that only an admin has:

```go
		v := wallet.Value()
		v.Pin = sealed.ComparedOf(req.NewPin)
		if _, err := wallets.Update(ctx, wallet, v); err != nil {
			return nil, err
		}
```

Use a longer PIN where you can.

To set a new PIN, give a new Compared to the value and update the row. The
fill of `change-pin` does this:

```go
		v := wallet.Value()
		if !v.Pin.Matches(req.OldPin) {
			return nil, commands.ChangePinWrongPin
		}
		v.Pin = sealed.ComparedOf(req.NewPin)
		if _, err := wallets.Update(ctx, wallet, v); err != nil {
			return nil, err
		}
```

The update checks the declared length of the new PIN, then stores its hash.
A PIN that breaks the length is 400 `invalid`, with the field `pin` and no
value.

## The handle of a command and a worker

### The repository of an entity

A command fill and a worker fill get a repository for each entity in
`writes`, such as `commands.WalletRepository(h)` or
`workers.DailyTotalRepository(h)`. A worker fill gets a reader for each entity
in `reads`, such as `workers.WalletReader(h)`, with only `Get` and `List`,
and values with no secret field.

Two kinds of entity have repositories of different forms:

- A child entity has no resource type of its own and a reference to an
  entity with one, such as a line item under an order. Its rows have no
  permissions of their own: each locked read, update and delete decides on
  the row of its parent, so each of these takes the loaded parent row.
- Each other entity has its own resource type, also when it has a reference
  to another entity, such as a Parcel under a Shipment. Its rows have their
  own permissions, so its locked reads, update and delete take no parent.

The repository of an entity with its own resource type is the generated type
`<Entity>Repo`, such as `commands.WalletRepo`. It has these methods:

| Method | What it does | The decision that it makes |
|---|---|---|
| `Get(ctx, id)` | Reads the row with no lock. | The read permission of the entity (`permissions.read`), for the caller of a command; the permission of the worker, for a worker. |
| `GetForUpdate(ctx, id)` | Reads the row and locks it with `FOR NO KEY UPDATE` until the transaction ends. | The read permission, then the declared `permission` of the command on the row. A worker decides its own permission. |
| `GetForDelete(ctx, id)` | As `GetForUpdate`, with `FOR UPDATE`. Use it before a delete, or before a change of a key column. | As `GetForUpdate`. |
| `Create(ctx, value, parent...)` | Inserts a row, writes its audit row and, in a service with change events, stages its event. `parent` is the loaded row of each reference of the new row. | For each reference of the row: the `permission` that the reference declares, else the permission of the command, on the referenced row. A command create of a row with no reference is covered by the decision before the fill. A worker create of a row with no reference is decided on the tenant resource. |
| `Update(ctx, from, value)` | Writes the value over the loaded row `from`, only while the row still has the version of `from`. The value keeps each reference of the row. | The permission of the command on the row, or of the worker. |
| `Move(ctx, from, value, parent...)` | As `Update`, for a value that changes a reference. `parent` is the loaded row that each changed reference names. Only an entity with a reference has it. | As `Update`, and for each changed reference the decision of `Create` on its new row. |
| `Delete(ctx, from)` | Deletes the loaded row `from`, with the same version check. | As `Update`. |
| `List(ctx, list, limit, args...)` | Reads the rows of a declared list, at most `limit`, with no lock. | Keeps only the rows on which the caller has the list permission of the entity (else its read permission), or the worker its permission. |
| `ListForUpdate(ctx, list, nil, args...)` | Reads the rows of a declared list with `lock: true`, and locks the allowed rows. The parent is `nil`. | As `List`. |

The repository of a child entity is `*command.Repository[<Entity>]`. Its
`GetForUpdate(ctx, id, parent)`, `GetForDelete(ctx, id, parent)`,
`Create(ctx, value, parent)`, `Update(ctx, from, value, parent)`,
`Delete(ctx, from, parent)` and `ListForUpdate(ctx, list, parent, args...)`
take the loaded row of the parent, which the fill read through its own
handle, such as `updated` in the create of a transaction above. The
decisions are those of the table, on the row of the parent. An entity with
no resource type and no reference also has the repository
`*command.Repository[<Entity>]`, and each of its methods takes no parent: pass
none.

A parent that the repository does not take does not compile: `go build` and
`stego test` give `too many arguments in call to parcels.Update`. For an
update that keeps the references of the row, give no parent. For an update
that changes a reference, call `Move` with the new referenced row.

Each read of a row decides a permission. This is also true for a row other
than the resource member of the command. In the example, the `transfer` fill
reads the target wallet with `GetForUpdate`: the caller must have `view` and
`operate` on the target wallet too. A row that is not there and a row that the
caller may not read give the same error, `command.ErrNotFound`, and no data of
the row. A fill that returns this error gets the answer 404 with the code
`not_found`. For a worker, the error of a row that it may not read or write also
matches `command.ErrRefused`, so a worker fill can tell a deny from a missing
row.

Other errors of the handle:

| Error | When |
|---|---|
| `command.ErrConflict` | The row changed since the fill read it, or a lock or a unique key failed. The runtime runs the fill again when the command sets `retry_on_conflict`, else the answer is 409. |
| `command.ErrInvalid` | A value breaks a declared constraint. The answer is 400 `invalid` with the field. |
| `command.ErrClosed` | The fill uses the handle after it returned, or with no handle. |
| `command.ErrInternal` | A fault of the fill or of the generated code, such as a loaded value of another transaction. |

After `ErrConflict`, `ErrInvalid` or `ErrInternal`, each later method of the
handle gives the same error, and the runtime commits nothing.

In one case the runtime runs a fill again also when the command sets no
`retry_on_conflict`: once, when the contract step of a seal dropped a clear
column while the call ran (see [Seal steps](schema-changes.md#seal-steps)). The
rerun rolls back each write of the first attempt, but an effect of the fill
outside the handle, such as a call to another system, runs twice.

A fill that returns any other error gets the answer 500 with the code
`internal`. The log then has one error line `command.fill.failed` with the
command and a fixed cause class: `fill_error` for an error of the fill
itself, `closed`, `internal`, `sealed` for a secret value that the key ring
does not open, `panic` or `deadline`. The line holds no error text, so put no
value of a row in the error. A line of a failed statement or of an internal
error that already names the cause replaces it. The log writes at most one
line for each command and cause class in ten seconds, with `held_back`.

### Map an error of the handle to a refusal

A fill that returns `command.ErrNotFound` gets the answer 404 with the code
`not_found`, or `NotFound` over gRPC, and the transaction rolls back. The
answer does not tell the client which row is not found. When a fill reads a
row other than the resource member, test the error with `errors.Is`, and
return a declared refusal that names it. The fill of `transfer` does this for
the target wallet:

```go
		to, err := wallets.GetForUpdate(ctx, req.ToWalletId)
		if errors.Is(err, command.ErrNotFound) {
			// The target wallet is not there, or the caller may not operate it.
			return nil, commands.TransferNoTarget
		}
		if err != nil {
			return nil, err
		}
```

`command` is the package `github.com/jsell-rh/stego/registry/runtime/command`.
The answer is then 422 with the code `transfer.no_target`. The refusal tells
the same for a missing wallet and for a wallet of another customer, so it
tells nothing of the rows of another customer.

You do not need this for the resource member of the command: the runtime reads
that row before the fill runs, and answers 404 when it is not there or the
caller may not act on it.

### Who writes the relations of a new row

The runtime writes these SpiceDB relations of a new row, after the commit:

- Each relation of `relationships` of the entity. With `subject_source:
  caller`, the subject is the caller of the create, such as the `owner` of a
  Customer. With `subject_source: field`, the subject is the ID in the field
  that `subject_field` names.
- The relation of each reference to its referenced row, such as
  `wallet:<ID>#customer@customer:<ID>`.
- The relation `tenant` to the tenant resource, when the type has the relation
  `tenant`.

No other code writes the `owner` of a row. Thus an entity whose permissions
read `owner` must declare the relationship:

<!-- excerpt -->
```yaml
    relationships:
      - {relation: owner, subject_source: caller, subject_type: user}
```

With no `relationships`, no create writes `owner`, and only a command with
`relationships_written` can write it. A command cannot write a relation of
`relationships`, the relation of a reference, or `tenant`.

The relay writes the tuples after the commit, often in less than one second.
Until then, a write that is decided on the new row gets 404. See [Write a
child at once after its
parent](commands-queries-workers.md#write-a-child-at-once-after-its-parent).

### A command on the service-level resource

A command whose `resource_type` is the resource type of the provisioning,
such as `service`, has no resource member. The runtime decides it on the
service-level resource of the tenant of the request. Its handle is another
type: it has `Create<Entity>` and `Get<Entity>` for each entity that the
command writes, and no repository, locked read, update, delete or list. The
fill of `start-campaign`:

```go
// New gives the fill of the command start-campaign.
func New() commands.StartCampaignFill {
	return func(ctx context.Context, req *commands.StartCampaignRequest, h *commands.StartCampaignHandle) (*commands.StartCampaignResponse, error) {
		campaign, err := h.CreateCampaign(ctx, commands.Campaign{Title: req.Title})
		if err != nil {
			return nil, err
		}
		return &commands.StartCampaignResponse{CampaignId: campaign.ID()}, nil
	}
}
```

The form `commands.CampaignRepository(h)` does not compile with this handle.
An entity that such a command writes cannot have a parent. The caller needs
`start_campaign` on `service:<base64url of the tenant ID>/<base64url of
wallets>`, which the schema gives to `admin`. Provision writes the first
admins (see [A command with no resource
member](commands-queries-workers.md#a-command-with-no-resource-member)).

### Rows with no parent, and who reads them

The worker `sum-balances` creates a DailyTotal row with no parent:

```go
// New gives the fill of the worker sum-balances.
func New() workers.SumBalancesFill {
	return func(ctx context.Context, tick time.Time, h *workers.SumBalancesHandle) error {
		wallets, err := workers.WalletReader(h)
		if err != nil {
			return err
		}
		funded, err := wallets.List(ctx, workers.SumBalancesFunded, 1000)
		if err != nil {
			return err
		}
		total := workers.DailyTotal{Wallets: int32(len(funded))}
		for _, w := range funded {
			total.Balance += w.Value().Balance
		}
		totals, err := workers.DailyTotalRepository(h)
		if err != nil {
			return err
		}
		_, err = totals.Create(ctx, total)
		return err
	}
}
```

The runtime relates such a row only to the tenant resource. Thus a person can
read it only through a permission of the type `tenant`, such as `audit` in the
schema of the example:

<!-- excerpt -->
```zed
definition daily_total {
	relation tenant: tenant
	permission view = tenant->audit
	permission record = tenant->worker_sum_balances
}
```

The type `tenant` of the example declares the relation `service` of the
provisioning resource type. Then each Provision relates the tenant resource
to the service-level resource of the same tenant, which holds the admins of
the Provision, and each Retire deletes the relation (ST-SUP-RC27-OC18). The
permission `audit` gives the rows to these admins and to each `auditor`:

<!-- excerpt -->
```zed
definition tenant {
	relation service: service
	relation auditor: user
	relation worker_sum_balances: service_account
	permission audit = auditor + service->admin
	permission record = worker_sum_balances
}
```

Validate refuses the relation `service` when the declaration has no tenant
provisioning, or when the relation admits another subject type than the
provisioning resource type. A schema with no relation `service` on the type
`tenant` gets no such relation.

No route and no declaration writes the relation `auditor` of a person on the
tenant resource. To give the rows to a person who is not an admin of the
Provision, an operator writes the grant in SpiceDB, on the tenant-qualified ID
of the tenant resource. For the tenant `acme` and the user `dora` of the
issuer `https://idp.example.test`:

```bash
b64() { printf %s "$1" | base64 | tr '+/' '-_' | tr -d '=\n'; }
t=$(b64 acme)
zed relationship create "tenant:$t/$t" auditor "user:$(b64 https://idp.example.test)|$(b64 dora)"
```

A grant on `tenant:acme` gives nothing. The ID of a user is the base64url of
the issuer, a `|`, and the base64url of the `sub` claim. The issuer must be
the `iss` claim of the token byte for byte: with another text, such as one
with a `/` at the end, the grant names another user and gives nothing.

The audit of the service does not see a grant that you write with `zed`.
Record each such grant, who wrote it and why, in your own change log. To
remove it, run `zed relationship delete` with the same three arguments. Retire
of the tenant also removes it.

In place of this, you can give the rows a parent that people can be granted
on, such as a ledger row that a command on the service-level resource
creates. Then the worker creates each row under that parent, and the parent's
permissions decide the reads.

### The lists of a command and a worker see only rows with a tuple

A list of a command keeps only the rows on which the caller has the list
permission of the entity (else its read permission). A worker list keeps only
the rows on which the worker has its permission. A row with no tuple in
SpiceDB, such as a row that a script wrote past the service, or a new row
whose tuples the relay did not write yet, is not in the list. Thus
`sum-balances` does not count a wallet that a script inserted into the table.
Write the rows only through the service.

The relay writes the tuples of a create after its commit, often in less than
one second. Thus a command right after a create can miss the new row: its
list does not hold the row, and `Get` of the row gives `command.ErrNotFound`.
For example, a client that creates a record of a patient and at once sends a
command that closes the open records of the patient can close every record
but the new one. To decide the command after the tuples of the create, send
the `Authz-Revocation-Location` of the create in the header
`Authz-Revocation-After` of the command (see [Write a child at once after its
parent](commands-queries-workers.md#write-a-child-at-once-after-its-parent)).
The command then waits for the tuples before its decision and its lists.

## The handle of a query

A query fill gets a read-only handle. For each entity in `reads`, the
contract package has a function `queries.<Query><Entities>(h)` that gives the
repository of the entity, such as `queries.DailyTotalsDailyTotals(h)` for the
query `daily-totals`, which reads `DailyTotal`. The function refuses an
entity that the query does not read. A query repository has two methods:

| Method | What it does | The decision that it makes |
|---|---|---|
| `Get(ctx, id)` | Reads the row of the ID in the tenant of the caller, in the read-only transaction of the query. It gives a `queries.Loaded<Entity>`; `Value()` gives the value. | The read permission of the entity on the row. A row that is not there and a deny both give `queries.ErrNotFound`, with no data of the row. |
| `List(ctx, request)` | Reads one page of the rows in the tenant: at most `request.Limit` values (0 is 100, the most is 1,000) after the cursor `request.After`, in the order `request.Order` and then by key, with only the rows whose fields equal each `query.Equal` of `request.Where` (at most 8). It gives a `queries.ListResult`: `Items`, `More` and `Next`, the cursor of the next page. | Keeps only the rows on which the caller has the permission of the query, for a list query, else the list permission of the entity. |

The fill of a list query gets the cursor and the limit of the request in
`req.After` and `req.Limit`. Give them to `List`, and give back `Next` and
`More`, so the client gets the cursor of the next page. A field of
sensitivity `pii` or `secret` cannot be in `Where`. A read of an entity with a
`pii` field writes its audit row before the method returns. A query never
writes: the handle has no write method, and the transaction is read only.

## The imports of a fill

A Go file of a fill that is built into the service imports only these
packages: the slot contract of the fill (such as `<module>/out/commands`)
and the runtime packages that the contract uses, the fill modules that the
declaration lists in `fill_modules`, and the fill allow list of the Go
standard library:

`bytes`, `cmp`, `context`, `crypto/sha256`, `encoding/base64`,
`encoding/hex`, `encoding/json`, `errors`, `fmt`, `hash`, `hash/crc32`,
`iter`, `log`, `log/slog`, `maps`, `math`, `math/big`, `math/bits`,
`regexp`, `slices`, `sort`, `strconv`, `strings`, `time`, `unicode`,
`unicode/utf16` and `unicode/utf8`.

`stego test` and `stego validate` refuse any other import, such as
`crypto/subtle`, `os`, `net/http` or `database/sql`, with a fault that names
the file and the import. They also follow the imports of a fill module, and
refuse a database driver in it. Some functions of the allowed packages are
refused too: `fmt.Print`, `fmt.Printf` and `fmt.Println`; the functions of
`log` and `log/slog` that change, stop or go past the standard logger, such
as `log.Fatal` and `slog.SetDefault`; and `time.AfterFunc` and
`context.AfterFunc`, which start a goroutine. They also refuse, in the fill
and in each fill module, a function with no body and the directives
`//go:linkname`, `//go:cgo_` and `//go:embed`. Thus a fill cannot use
`reflect`, `unsafe` or `//go:linkname` to read the memory of the runtime,
such as the stored hash of a counted PIN. A test file, whose name ends in
`_test.go`, is not built into the service, so it can import any module of
`go.mod`.

A fill does not import `crypto/rand` or `math/rand`, so that its review finds
each source of randomness by name. To issue a secret, such as an API key or a
one-time code, call `command.NewToken(size)` of the package
`github.com/jsell-rh/stego/registry/runtime/command`, which the contract of a
command fill and of a worker fill always imports (from runtime v0.163.0). It
gives `size` new random bytes from `crypto/rand`, from 16 to 64, and refuses
another size with an error. Encode the bytes with `encoding/base64` or
`encoding/hex`, or take a code of digits with `math/big`:

```go
raw, err := command.NewToken(32)
if err != nil {
	return nil, err
}
apiKey := base64.RawURLEncoding.EncodeToString(raw)
```

For a code of digits, read all the bytes as one number and reduce that number
once. Do not take one digit from each byte with `b % 10`: 256 is not a
multiple of 10, so the digits 0 to 5 come more often than 6 to 9. With 32
random bytes, the bias of one reduction is too small to measure:

```go
raw, err := command.NewToken(32)
if err != nil {
	return nil, err
}
n := new(big.Int).SetBytes(raw)
code := fmt.Sprintf("%06d", n.Mod(n, big.NewInt(1_000_000)))
```

Store such a code in a field with `sensitivity: secret` and
`compare_only: true`, so the table holds only its hash, and check it with
`sealed.Compared`. A code of 6 digits has only one million values, so limit
the wrong attempts too (see [Check a compare-only
secret](#check-a-compare-only-secret)).

`command.NewID()` gives a new random UUID in its text form, with 122 random
bits. Use it for a new ID. Do not make a secret from an ID, the time or the
input with `crypto/sha256`: each person who knows the inputs can make the
same value again.

Thus a fill cannot import `crypto/subtle` to compare two secret values.
Prefer a compare-only field and `sealed.Compared` (see [Check a compare-only
secret](#check-a-compare-only-secret)): it checks a value against the stored
hash with no timing leak. When the fill must compare two values that it has,
compare their SHA-256 digests with `crypto/sha256`. Then the time of the
compare does not show the values, but it can show how much of the two
digests is the same.

A fill does not import `google.golang.org/protobuf` either. To give a member
of the type `google.protobuf.Struct` in a command or a worker message, such as
the summary of an export, call `command.NewStruct(fields)` (from runtime
v0.175.0). It takes a `map[string]any` and gives the `*structpb.Struct` of the
member, with no import of `structpb` in the fill:

```go
summary, err := command.NewStruct(map[string]any{
	"period": req.Period,
	"sites":  []any{"s-1", "s-2"},
	"counts": map[string]any{"enrolled": 12, "withdrawn": 1},
})
if err != nil {
	return nil, err
}
return &commands.ExportPeriodResponse{Summary: summary}, nil
```

Each value is `nil`, a `bool`, a Go number, a `json.Number`, a `string`, a
`[]byte` (it becomes base64 text), a `map[string]any` or a `[]any`. A number
becomes a JSON number, which holds an integer exactly only up to 2^53: give a
larger integer, such as an ID, as a string. `NewStruct` refuses another type,
a key or a string that is not valid UTF-8, and objects and lists nested more
than 64 levels, such as a map that holds itself, with an error. A member of
the type `google.protobuf.Value` or `ListValue` is a value of the `Fields` of
such a Struct, such as `summary.Fields["sites"].GetListValue()`. To read such a
member of a request, call its methods, such as `req.Filter.AsMap()`. The
OpenAPI document marks a command answer that holds such a member
`x-stego-opaque`, so the CLI writes it only to `--output-file FILE`.

### Declare a fill module

To let the fills import a Go module, list it under `fill_modules` at the top
level of the declaration, with the reason that the fills need it, so a
reviewer can judge it:

```yaml
fill_modules:
  - {module: github.com/google/uuid, reason: fills make request IDs}
```

Then add the module to `go.mod` with `go get`, as for any module. A fill can
import each package of a listed module. The rules:

- `module` is the module path, which has a dot in its first element. A
  module whose path only starts with a listed path, such as
  `github.com/google/uuidx`, is not listed.
- `stego test` and `stego validate` follow the imports of the module too, and
  refuse the fill when the module reaches a refused package, such as
  `database/sql` or `os`, or prints, with a fault that names the chain of
  imports.
- A test file of a fill does not need the list: it can import any module of
  `go.mod`.

## Change events

With the mixins `durable-events` and `kafka-events`, each create, update and
delete of an entity stages one change event in the transaction of its write.
The worker mode sends it to Kafka. `kafka-events` needs `durable-events`:

<!-- excerpt -->
```yaml
mixins:
  - durable-events
  - kafka-events
```

The value of the Kafka record is this JSON object, and nothing else:

```json
{"entity":"Wallet","action":"updated","id":"01a1efce-4159-74fc-81cd-da9fb5696bb8","data":{"balance":90,"customer_id":"01a1efce-4100-7aa0-8c11-0b5a2e3f4d61","label":"main"},"data_version":1}
```

| Member | What it is |
|---|---|
| `entity` | The name of the entity, as the declaration gives it. |
| `action` | `created`, `updated` or `deleted`. |
| `id` | The ID of the row. |
| `data` | Only for an entity with `events`: the declared fields of the row by their names. See below. |
| `data_version` | Only for an entity with `events`: the version of the fields. |

The record has the headers `stego-event-id`, `stego-event-type` (the event
kind, such as `wallets.wallet.updated`), `stego-event-action`,
`stego-tenant` and `stego-idempotency-key`. Its key is `<tenant
ID>\x1f<resource type>:<row ID>`. The header `stego-idempotency-key` is a
SHA-256 over the tenant, the caller, the `Idempotency-Key` of the request and
the event, and the audit row holds that key as the client sent it. Thus a client must
never put a personal value, such as an email, in an `Idempotency-Key`.

A worker that consumes the subscription of these events in another service
gets this object as its message, and the event kind as its effect name:

```go
// Change is the payload of a change event of a wallet.
type Change struct {
	Entity      string `json:"entity"`
	Action      string `json:"action"`
	ID          string `json:"id"`
	DataVersion int    `json:"data_version"`
	Data        struct {
		CustomerID string `json:"customer_id"`
		Label      string `json:"label"`
		Balance    int64  `json:"balance"`
	} `json:"data"`
}
```

```go
	return func(ctx context.Context, message json.RawMessage, effect string, h *workers.RecordWalletsHandle) error {
		var c Change
		if err := json.Unmarshal(message, &c); err != nil || effect != "wallets.wallet.updated" {
			return workers.Refused(ErrBadMessage)
		}
		// c.ID is the ID of the wallet in the service wallets, and c.Data.Balance its balance after the write.
		return nil
	}
```

### The fields of a change event

A worker handle has no network client, so a consuming fill cannot read a row
from the service `wallets`. Thus an entity declares the fields that its change
events carry (runtime v0.176.0, rest-api 4.32.0):

<!-- excerpt -->
```yaml
    events:
      fields: [wallet_id, kind, amount]
```

- `fields` names one to 64 fields of the entity, each once. The event of a
  create and of an update holds the fields of the row after the write, and the
  event of a delete the row before the delete. A field with no value is
  `null`. A timestamp is an RFC 3339 text in UTC, bytes are base64, and a
  `jsonb` field is its JSON value.
- A `secret` field cannot be in `fields`.
- A `pii` field needs a reason in `pii_reasons`, such as `{email: the ledger
  sends each customer a receipt}`. The audit rows of the service redact the
  field, but the event holds it in clear for each consumer of the topic.
- `version` is a whole number from 1, and 1 when you leave it out. The event
  gives it as `data_version`.

An entity with no `events` gives no `data`: the event holds no field value,
also no `public` field. `stego validate` refuses `events` in a declaration
with no `kafka-events` mixin.

A consumer reads the fields as a contract. Thus `stego compat`, and the check
against the base of `stego plan` and `stego apply`, refuse a head that removes
a field from the list of the base, gives it another type, removes a value of
an enum field, lowers the version or removes `events`. A field that you add is
accepted. To make one of the other changes, raise `version`, such as to 2, and
tell the consumers. The fault is `ST-GEN-INV39`:

```text
fault ST-GEN-INV39 Transaction.amount: the change events of Transaction carry the event fields of version 1 in the base, which a consumer reads as a contract, and the head leaves out the field amount; keep the field with its type, or raise events.version to 2, which tells each consumer of a breaking change (ST-GEN-INV39)
```

## Test a fill

`stego test` runs `go test` on each fill and records the evidence. It exits
non-zero when a test fails, and also when it records no evidence for a fill:
for example, a fill with no test file, a refused import, or a contract that
cannot be generated because the declaration is not valid. The line of the fill
names the cause. So a CI step of `stego test` fails then. A unit test
cannot give a fill a real handle: STEGO has no test handle, and a handle needs
the transaction, the tenant, the caller and the decisions of SpiceDB. With no
handle, each method of a command or worker repository gives
`command.ErrClosed`, and a query repository gives an error.

Thus write a fill in two parts:

1. Put the business rules in plain Go functions that read no handle, such as
   `Withdraw` above. Test them with `go test`, with all the cases.
2. Keep the code that uses the handle short, and prove it through the running
   service: start the service with a local PostgreSQL and a local SpiceDB,
   provision a tenant, and send the requests.

A test of a rule:

```go
func TestWithdraw(t *testing.T) {
	cases := []struct {
		name            string
		pin             bool
		balance, amount int64
		want            int64
		err             error
	}{
		{"enough", true, 100, 40, 60, nil},
		{"all", true, 100, 100, 0, nil},
		{"short", true, 100, 101, 0, commands.WithdrawShortBalance},
		{"wrong pin", false, 100, 1, 0, commands.WithdrawWrongPin},
		{"zero", true, 100, 0, 0, commands.WithdrawInvalidAmount},
		{"below zero", true, 100, -1000, 0, commands.WithdrawInvalidAmount},
		{"below zero, wrong pin", false, 100, -1, 0, commands.WithdrawInvalidAmount},
	}
	for _, c := range cases {
		got, err := Withdraw(c.pin, c.balance, c.amount)
		if !errors.Is(err, c.err) || (err == nil && got != c.want) {
			t.Errorf("%s: Withdraw = %d, %v; want %d, %v", c.name, got, err, c.want, c.err)
		}
	}
}
```

A test can make a stored hash, to test a check of a compare-only secret:

```go
func TestPin(t *testing.T) {
	hash, err := sealed.Hash("4711")
	if err != nil {
		t.Fatal(err)
	}
	stored := sealed.ComparedHash(hash)
	if !stored.Matches("4711") || stored.Matches("4712") || stored.Matches("") {
		t.Fatal("the PIN check is wrong")
	}
}
```

A test can also call the fill with no handle, to prove that it reads through
the handle first:

```go
func TestNoHandle(t *testing.T) {
	_, err := New()(context.Background(), &commands.WithdrawRequest{WalletId: "w", Amount: 1, Pin: "4711"}, nil)
	if !errors.Is(err, command.ErrClosed) {
		t.Fatalf("the fill with no handle gave %v, want command.ErrClosed", err)
	}
}
```

For a command on the service-level resource, give
`commands.NewStartCampaignHandle(nil)` in place of the handle.
