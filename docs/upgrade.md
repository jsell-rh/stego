# Upgrade a STEGO service

This guide tells an operator how to move a running service to a newer STEGO
release. It gives the general procedure, the release notes of each change
that needs an action from you, and the steps of each action. For the
operations after the upgrade, such as a rotation of a secret or a restore,
see [Day-2 operations](operations.md).

A release of STEGO is a commit of this repository. The archetypes
`rest-crud`, `rpc-service` and `browser-service` have their own versions. The
release notes below name the first archetype versions that hold each change.
The version of an archetype is in `registry/archetypes/<archetype>/archetype.yaml`,
and `.stego/state.yaml` of your project records the versions that your last
`stego apply` used. Some notes name a version of the STEGO runtime library
instead. Your service uses the version that the line
`github.com/jsell-rh/stego/registry/runtime` of its `go.mod` names.

## The procedure

Do these steps for each upgrade. Do them in this order.

1. Find the archetype version that your service runs now. Read each release
   note below whose version is after it and at or before the target version.
   Do the action of each note in the order of the table.
2. Take a backup of the database of the service. See [Restore from a
   backup](operations.md#restore-from-a-backup) for what the backup must hold.
3. Make the new release of the service. Use the new `stego` binary on the
   declaration of the release that runs now, and give `--base` the commit or
   tag of that release. Run the commands in this order:

   ```sh
   stego test
   stego fill sign <fill>
   stego validate
   stego compat --base <tag of the running release>
   stego plan --base <tag of the running release>
   stego apply --base <tag of the running release>
   stego deps
   ```

   Run `stego test` first. A new STEGO release can change the contract of a
   fill, such as the command contract, also when you do not change the fill.
   Then the evidence of the fill is stale, and `stego validate`, `stego plan`
   and `stego apply` refuse it: `fill "<fill>" is stale ... the contract
   command.command changed after its evidence; run stego test, then stego
   fill sign <fill>`. `stego test` runs the tests of each fill on the new
   contracts and records new evidence. Review each fill, its diff and the
   change of its contract, then run `stego fill sign <fill>` for each fill
   whose evidence `stego test` recorded. When a test does not
   compile, such as with `too many arguments in call to`, do the action of
   its release note (step 1) and run `stego test` again. When `stego plan`
   says that the declaration needs add steps and saved no plan, run `stego
   apply --base` without `--saved-plan`. See [Schema changes](schema-changes.md). When a
   Go build fails with `could not import log/slog`, see [A Go build fails
   with "could not import log/slog"](#a-go-build-fails-with-could-not-import-logslog).
4. Make each new Secret, grant and role that the release notes name, before
   you deploy. A pod that needs a Secret key that is not there does not start.
   `stego env check` finds a missing Secret of the environment file:

   ```sh
   stego env check --requirements deploy/platform-requirements.yaml env/<environment>.yaml
   ```

5. Deploy the release. The migration Job runs first, then the Deployments.
   See [The migration Job](delivery.md#the-migration-job).
6. Look at the result:
   - `kubectl rollout status` of `<service>` and of `<service>-worker`.
   - The log of each pod: a line `the database does not satisfy the schema
     manifest` is an alert. A pod of the new release must not write it.
   - The parked list of each destination of a consuming worker (see [Add a
     subscription to a running service](#add-a-subscription-to-a-running-service)).
7. If you must go back, go back by one release only. See [Which older releases
   can run on a newer database](operations.md#which-older-releases-can-run-on-a-newer-database).

Do not skip a release that holds a contract step of a schema change. `stego
compat` compares your change with the base that you give it. It cannot see a
release that you did not deploy.

## The clients of the release before

During the rollout, the release before and the new release serve the same
routes. After the rollout, the clients that you built for the release before
still call the service until you build them again. This section tells what
such a client gets from the new release. In walkthrough 18, the HTTP and the
gRPC clients of a release with runtime v0.138.0 worked against a release with
runtime v0.165.0, except for the cases below.

- **An idempotency key that the release before stored.** A request again
  with the same key, method, path and body gets the stored answer while the
  service keeps the key, 24 hours by default. A request with the same key and
  another path or body gets 422 with the problem type
  `idempotency-key-reused`. A request with the same key and the same body
  also gets 422 when the new release makes the fingerprint of the request in
  another way: when the release before had no key ring and the new release
  has one, such as from 3.189.0 for a service with a `pii` field or from
  3.211.0 for a service with a `debug_redact` member, or when the key ring no
  longer holds the key that made the fingerprint. The answer carries
  `Idempotency-Kept: 1` from 3.216.0. After a 422, do not send the key again.
  Read the state of the resource, and send a new write with a new key only
  when the first write was not done.
- **A field that a retype changes.** The new release reads and writes the
  field in its new JSON type, such as `"7"` in place of `7`. It refuses a
  write of the old type with 400 and the problem type `validation-error`,
  which names the field. A client must read both types while the two
  releases run, and write the new type only to the new release. A require
  also refuses a create that does not send the new field. See [Retype and
  require](schema-changes.md#retype-and-require).
- **A member that the entity does not take.** From 3.227.0, a create, an
  update and an upsert refuse such a member, such as a field that a drop
  removed, a typo or a name in another letter case, with 400 and the problem
  type `validation-error`. The problem names the member, and the write
  changes nothing. From 3.215.0, a patch refuses such a member in the same
  way. Before 3.227.0, a create and an update ignored the member and answered
  as for a body without it: a create gave 201, and the value was lost. See [A
  client that still sends the dropped field](schema-changes.md#a-client-that-still-sends-the-dropped-field).
- **An idempotency key that is not a structured string.** From 3.235.0, a
  request whose `Idempotency-Key` header is there, but is not one structured
  string of 1 to 255 characters, gets 400 with the problem type
  `idempotency-key-invalid`, and the write changes nothing. A gRPC command
  gives the message `idempotency-key-invalid`. Before, such a key got
  `idempotency-key-missing`, which is now only for a request with no header.
  Send the key in double quotes, such as `"k-1"`. A client of the release
  before that reads only `idempotency-key-missing` must also read the new
  type.
- **A repeated apply of the CLI.** From 3.235.0, an apply of the same
  document, whose values the service already stores, reports `unchanged` and
  sends no patch. A CLI of the release before reports `configured` and sends
  the patch. A script that reads the status of an apply must accept both.
- **An entity that a drop removes.** From the expand step of the drop, the
  new release serves no route of the entity and its collections, so a
  request to such a route gets 404. The release before still serves them
  until it stops. See [Remove an entity](schema-changes.md#remove-an-entity).
- **A generated client of the release before.** The CLI and the SDKs hold the
  OpenAPI document of the release that you built them from. Thus they do not
  know a later change of the document:
  - A CLI before cli-application 1.17.0 does not read `Idempotency-Kept`. It
    sends a write again after a 5xx answer by the flag of its own document.
  - A CLI that you built before a member had the mark `debug_redact` gives
    that member a flag, which puts the secret in the shell history and the
    process list. A CLI that you built before a command was Sensitive writes
    its answer to standard output.
  - A client that you built before a retype sends and reads the old JSON
    type of the field.

  Build the CLI and the SDKs again from the OpenAPI document of each new
  release, and give the new clients to their users before you stop the
  release before. A CLI of the new release that calls the release before
  writes a warning to standard error when an answer has no
  `Idempotency-Kept` header. The warning changes no answer and no exit code.
- **A gRPC client.** A gRPC answer has no `Idempotency-Kept` metadata. After
  a failure, send the command again only with the same key: the service then
  gives the stored answer, or runs the command once.

## Release notes

Each row is a change that needs an action from you. The versions are the
first versions of the archetypes that hold the change. A dash means that the
archetype does not have the change.

| rest-crud | rpc-service | browser-service | Change | Action |
|---|---|---|---|---|
| 3.147.0 | 1.147.0 | 1.149.0 | browser-backend 4.6.0. The binary of a browser service has the mode `migrate`. The migration Job makes the session table and checks the database roles. The check needs a NOLOGIN owner role and a migration login in it. | [Move a browser-service database to the owner and migrator logins](#move-a-browser-service-database-to-the-owner-and-migrator-logins) |
| 3.160.0 | 1.161.0 | 1.163.0 | `stego init` writes `go.mod`, and each later command reads the module path from it. `STEGO_MODULE` has no effect. | [Remove STEGO_MODULE](#remove-stego_module) |
| 3.169.0 | 1.169.0 | - | The base has two Deployments: `<service>` runs the api mode, and `<service>-worker` runs the worker and controller modes. Both hold `DATABASE_URL`. | Remove an overlay patch of `STEGO_MODE` (see [Probes and modes](delivery.md#probes-and-modes)). Roll both Deployments after each change of a database password ([Rotate a database password](operations.md#rotate-a-database-password)). |
| 3.173.0 | 1.175.0 | - | kafka-producer 3.3.0 and kafka-consumer 0.3.0 read the Kafka password and the CA file from the Secret `<service>-kafka-client`. | [Make the Secret of the Kafka client](#make-the-secret-of-the-kafka-client) |
| 3.176.0 | 1.178.0 | - | The worker Deployment runs as the ServiceAccount `<service>-worker`, not `<service>`. | [Give the worker ServiceAccount its grants](#give-the-worker-serviceaccount-its-grants) |
| 3.179.0 | 1.181.0 | 1.182.0 | `stego fill create` writes a test that does not name `ErrNotImplemented`. `stego test` names the cause when an old scaffold test still expects it. | [Write the tests of your fills](#write-the-tests-of-your-fills) |
| runtime v0.134.0 | runtime v0.134.0 | - | A new consumer waits for its worker tuple, so a new subscription no longer parks its backlog. The refresh age of the OIDC key set is the `max-age` of the issuer. A second cursor key can open cursors. A route that does not exist answers 404. A retire removes the tuples of the rows of the tenant. A message that an older runtime counted toward poison keeps its count. | [Add a subscription to a running service](#add-a-subscription-to-a-running-service), [Rotate the OIDC signing key](operations.md#rotate-the-oidc-signing-key), [Rotate the cursor key](operations.md#rotate-the-cursor-key). Before the upgrade, resolve each parked message of a consuming worker (see [See and resolve the parked messages](commands-queries-workers.md#see-and-resolve-the-parked-messages)): a message that the older runtime almost parked can park soon after the upgrade. |
| 3.182.0 | 1.184.0 | - | Checking refuses a `route` on a query whose `method` is a gRPC server stream: no handler served it. The query contract names the repository of an entity with the plural of its table, such as `GetEntryEntries`; the old name, such as `GetEntryEntrys`, stays one release as a deprecated function. Runtime v0.136.0: a write waits for the revocation that `Authz-Revocation-After` names, and a unique violation names the fields of its key. | [Remove the route of a stream query](#remove-the-route-of-a-stream-query), [Use the new names of the query repositories](#use-the-new-names-of-the-query-repositories). A client that creates a child at once after its parent can send `Authz-Revocation-After` (see [Write a child at once after its parent](commands-queries-workers.md#write-a-child-at-once-after-its-parent)). |
| runtime v0.137.0 | runtime v0.137.0 | - | A field with `sensitivity: secret` is sealed at rest: the column of a readable secret holds sealed bytes (`bytea`), and a `compare_only` field holds an Argon2id hash. The api, worker and controller modes do not start without the key ring of the Secret `<service>-secret-key-ring`. In a fill, the type of a secret field is `sealed.Secret` (read its text with `Reveal()`) or `sealed.Compared` (check a value with `Matches`). A field that a release before v0.135.0 declared secret was stored in clear: `stego compat` and `stego plan` refuse the release until you declare a seal step for it. | [Seal the secret fields of a release before runtime v0.135.0](#seal-the-secret-fields-of-a-release-before-runtime-v01350) |
| 3.189.0 | 1.190.0 | 1.191.0 | Runtime v0.138.0 and postgres-adapter 6.14.0: the seal step is built. A service with a `pii` field and no `secret` field also needs the key ring, which seals the stored answers of its idempotency keys. A retry of a key that the release before stored gets `idempotency-key-reused`. | [Make the key ring of a service with a pii field](#make-the-key-ring-of-a-service-with-a-pii-field) |
| 3.199.0 | 1.199.0 | 1.199.0 | Runtime v0.150.0, postgres-adapter 6.17.0, command 1.13.0 and rest-api 4.19.0. A process of the release of the expand step of a seal goes on after the contract step drops the clear column. `stego compat` and `stego plan` refuse the contract step of a seal over a base with a runtime before v0.150.0. The update request of an entity with a secret field requires no secret: an update that leaves it out keeps it. Checking refuses a field named `kind` or `href` of an entity that a collection serves. | Before the contract step of an open seal, deploy a release with runtime v0.150.0 or later and the seal still at expand (see [The contract step of a seal](schema-changes.md#the-contract-step-of-a-seal)). Give a field named `kind` or `href` another name: when the base stores it, add a field of another name and drop the old one, because a rename keeps the API name. |
| 3.206.0 | 1.206.0 | 1.206.0 | command 1.14.0: the repository of an entity with its own resource type is the type `<Entity>Repo`. Its `GetForUpdate`, `GetForDelete`, `Update` and `Delete` take no parent, and an update that changes a reference is `Move`. A fill that gives a parent to one of them does not compile: `too many arguments in call to`. Runtime v0.154.0: a process whose database lacks a table or a column of its manifest, such as after a roll back past a contract step, starts not ready; the api Deployment of a service with modes has the readiness `/readyz/modes`. After an offset reset, a record that comes again takes its place before the later messages of its row, and a message parked as poison goes back to pending when an earlier message of its row is done. | [Give no parent where the repository takes none](#give-no-parent-where-the-repository-takes-none). Remove an overlay patch of the readiness probe of `<service>`. See [Which older releases can run on a newer database](operations.md#which-older-releases-can-run-on-a-newer-database) and [Repair a wrong order with an offset reset](subscriptions.md#repair-a-wrong-order-with-an-offset-reset). |
| 3.209.0 | 1.209.0 | 1.209.0 | Runtime v0.157.0, postgres-adapter 6.19.0, command 1.15.0 and rest-api 4.21.0. A field written `sensitivity: public` is a field with no sensitivity: a list searches and orders it, and its table has its search index. A seal `from: public` keeps the search index of the field, as a seal with no `from`. `stego compat` and `stego plan` refuse the contract step of one seal of an entity while the base holds another seal of the same entity open. A process of the release of the expand step writes the clear column again when a restore brings it back, and the seal job completes no pass with no checkpoint while the clear column is there. | Declare the contract steps of the open seals of one entity in the same release (see [The contract step of a seal](schema-changes.md#the-contract-step-of-a-seal)). |
| 3.211.0 | 1.211.0 | 1.211.0 | rest-api 4.22.0, cli-application 1.13.0, command 1.16.0 and runtime v0.158.0: a member of a command message that the proto file marks `[debug_redact = true]` is a secret. The OpenAPI document gives it `x-sensitive: true`. The generated CLI gives it no flag: give it in `--body FILE`. A command whose answer holds such a member writes its answer only to `--output-file FILE`. A service with such a member reads the key ring, which keys the fingerprints of its idempotency keys and seals their stored answers. `stego validate` gives the warning `unmarked-secret-member` for a member of a command request that has the name of a secret field of an entity that the command writes and has no mark. The warning does not stop validate, plan or apply. | Mark each secret member of a command request and response, such as a PIN, with `[debug_redact = true]` (see [A secret member](commands-queries-workers.md#a-secret-member)). A service with no `secret` and no `pii` field then needs the key ring: make it as for a `pii` field (see [Make the key ring of a service with a pii field](#make-the-key-ring-of-a-service-with-a-pii-field)). Build the CLI again, and give each secret member in a body file. |
| 3.214.0 | 1.214.0 | 1.214.0 | rest-api 4.23.0, command 1.17.0, cli-application 1.15.0 and runtime v0.160.0. The runtime refuses to serve a command with a secret member when the service has no key ring. Generation refuses a route whose path parameter is a secret member. A command whose answer holds a secret member at any depth is Sensitive. A Sensitive command refuses `--output-file -`. The CLI refuses a `--body FILE` that holds a secret member when the group or other users have access to the file. | Make the key ring before you deploy a release with a secret member. Give a secret member in the body, not in the path. Use `chmod 600` on each body file that holds a secret. |
| 3.215.0 | 1.215.0 | 1.215.0 | rest-api 4.24.0: a patch refuses each body member that is not a patchable field, `kind`, or a field that a collection write cannot change. The names must match in letter case: a member such as `Title` for the field `title` is refused. The answer is a 400 validation problem that names the member, and the patch changes nothing. Before, a patch ignored such a member and answered 200. The OpenAPI document gives each `<Collection>PatchRequest` with `additionalProperties: false`. cli-application 1.16.0: an apply of the same documents again leaves out each create-only field whose value is the stored one. | Send in a patch only the fields that `patchable` names, in the letter case of the OpenAPI document, and after a rename the API name of the field. Build the clients again from the new OpenAPI document (see the operations of a collection in [Quick start](../README.md#quick-start)). |
| 3.216.0 | 1.216.0 | 1.216.0 | idempotency 0.2.0, command 1.18.0, rest-api 4.25.0 and 4.26.0, cli-application 1.17.0 and 1.18.0, go-sdk 2.2.1, typescript-sdk 1.2.3, http-application 2.3.1 and runtime v0.161.0. Each answer to a create, a patch or a command whose request carries an `Idempotency-Key` that the service keeps carries the header `Idempotency-Kept: 1`, also a problem and a 5xx answer. The header is for HTTP only: a gRPC answer has no such metadata. The generated CLI sends a write again after a 5xx answer only when the answer carries the header. A 2xx answer with no header to a write whose key the CLI expects kept gives a warning on standard error, because the CLI and the service can be of different releases. The OpenAPI document marks the root of a command answer `x-stego-opaque` when the message holds a `google.protobuf.Any`, `Struct`, `Value` or `ListValue` member at any depth. The CLI makes such a command Sensitive: its answer goes only to `--output-file FILE`. A query answer gets no such mark. The CLI refuses a token file, a configuration file and a secret body file that another user owns. | Build the CLI and the SDKs again from the new OpenAPI document. Make sure that no proxy in front of the service adds `Idempotency-Kept`: the CLI trusts the header to send a write again. Give a command with an opaque answer `--output-file FILE`. Never put a secret in a query answer: a call that makes a secret, such as an API key, is a command with a `debug_redact` member. Make the CLI user the owner of its token, configuration and body files. |
| 3.220.0 | 1.220.0 | 1.220.0 | grpc-application 2.4.0, cli-application 1.20.0 and runtime v0.163.0. Only the api Deployment gets `STEGO_GRPC_TLS_CERT` and `STEGO_GRPC_TLS_KEY` and mounts the Secret `<service>-grpc-tls`, which holds the private key of the gRPC server. The worker Deployment and the migration Job no longer get them. The two variables are not required on their own in the platform requirements: they are required when the process serves gRPC, so `stego env check` does not name a missing path of them. `APPLY_CONFIG`, the configuration file of the generated CLI, is in no pod, and the platform requirements do not list it. `command.NewToken(size)` gives a fill 16 to 64 random bytes from `crypto/rand` for a secret that the command issues. The contract of each command fill and worker fill now imports the `command` package, so a fill can call it also in a service with no secret field. `crypto/rand` stays out of the imports of a fill. | Remove each overlay patch that gives the gRPC TLS Secret, or `STEGO_GRPC_TLS_*`, to the worker Deployment or the migration Job. Keep the Secret `<service>-grpc-tls`, and give the api pods the paths of its files, or list the Component `secret-files` in the overlay. Remove `APPLY_CONFIG` from the environment file and the overlays, and set it only in the shell of the CLI user. Make each secret that a fill issues with `command.NewToken`, never from an ID, the time or a hash of the input (see [The imports of a fill](fills.md#the-imports-of-a-fill)). When `stego validate` names a fill as stale, run `stego test` and `stego fill sign <fill>` (step 3 of [The procedure](#the-procedure)). |
| 3.226.0 | 1.226.0 | 1.226.0 | Runtime v0.165.0. When the database refuses a prepared statement with SQLSTATE 0A000 `cached plan must not change result type`, such as after a person drops a column and adds it again while the service runs, the runtime makes each connection of the pool prepare its statements again. It runs a command once more in a new transaction, with no wait. A query answers 500 one time, and the next call passes. Thus a fill can run two times in one request. | Make each fill do no effect outside its transaction, such as a call to another system, or make that effect idempotent: send each change to another system as an effect through the outbox (see [Effects](commands-queries-workers.md#effects)). |
| STEGO commit 40f7f991f | STEGO commit 40f7f991f | STEGO commit 40f7f991f | The archetype versions stay 3.226.0 and 1.226.0; grpc-processes 1.8.0. `stego compat` and `stego plan` refuse a drop of a field whose seal is open in the base, and a drop of an entity that has such a field. The fault names the seal and gives its contract step as the change to declare. A drop of a field that the base stores secret, whose `definition` gives the field in clear, gets a fault that tells you to add the item `secret` to the definition. Another field of the entity can drop while the seal stays open. `stego env check` prints a notice for a path variable of the api mode that names the file of a secret, such as `STEGO_GRPC_TLS_CERT`, when the environment file gives it no value. The notice is not a failure. | Contract the seal first, with the step `{change: <seal>, phase: contract}` in a release of its own, then declare the drop in a later release (see [The contract step of a seal](schema-changes.md#the-contract-step-of-a-seal)). In the definition of the drop of a sealed field, add the item `secret` before a pattern. For each notice of `stego env check`, give the path of the file in the environment file, or list the Component `secret-files` in the overlay. |
| 3.227.0 | 1.227.0 | 1.227.0 | rest-api 4.27.0: a create, an update and an upsert refuse each body member that is not a field of the entity by its API name, `kind`, `id` or `href`. The names must match in letter case: a member such as `Title` for the field `title`, a typo, the declared name of a renamed field or `tenant_id` is refused. The answer is a 400 validation problem that names the member, and the write changes nothing. Before, the write ignored such a member and answered 2xx, and the field was not set. A body may still give `id`, `href` and each field that the server sets, as a read gives them: the server ignores these values. A body that gives an implicit field of the collection another value than the value of the collection is refused too; before, the value of the collection replaced it. The OpenAPI document gives each `<Entity>CreateRequest` and `<Entity>UpdateRequest` with `additionalProperties: false`, marks each member that the server sets `x-output-only`, and each member that the body may only repeat `x-kept`. cli-application 1.21.0, go-sdk 2.2.2 and typescript-sdk 1.2.4 take the new document. | Send in a create, an update or an upsert only the fields of the entity, in the letter case of the OpenAPI document, and after a rename the API name of the field. Leave an implicit field out, or give the value of the collection. Build the CLI and the SDKs again from the new OpenAPI document (see the operations of a collection in [Quick start](../README.md#quick-start)). |
| 3.232.0 | 1.232.0 | 1.232.0 | Runtime v0.168.0 and postgres-adapter 6.20.0. The seal checkpoint keeps the database and the start of the database server where the seal job last saw it. After a restore, or a restart of the database server, the first run of the seal job records that it saw the row and waits; a pass that has not started runs 3 minutes later, at the next run. Before, a restore kept the time of the row from the dump, and the 3-minute wait did not apply. A runtime before v0.168.0 does not keep the first sight of the row, so its controller never starts a pass on such a row. | After a restore of a database with an open seal, expect the first pass at the second run of the seal job, 10 minutes after the start of the worker (see [Restore from a backup](operations.md#restore-from-a-backup)). To roll back to a release with a runtime before v0.168.0 while a seal is open, run the migration Job of that release again before you start its worker; else its controller never starts a pass. |
| 3.235.0 | 1.235.0 | 1.235.0 | Runtime v0.171.0, idempotency 0.3.0, command 1.23.0, rest-api 4.31.0 and cli-application 1.24.0. A request whose `Idempotency-Key` header is there but is not one structured string of 1 to 255 characters gets 400 with the new problem type `idempotency-key-invalid`; `idempotency-key-missing` is now only for a request with no header. A gRPC command gives the message `idempotency-key-invalid` for such a key. The create-request schema of the OpenAPI document does not require a field with a declared default, such as `stock` with `default: 0`: a create that leaves it out stores the default. The generated Go SDK thus gives such a member of a create request as a pointer, and the generated CLI no longer requires its flag. The CLI names the missing field when `apply` refuses a document, gives the problem type and the failed fields of a 400 problem, and reports `unchanged` and sends no patch when an apply of the same document finds the stored values. | A client that reads the problem type of a 400 also reads `idempotency-key-invalid`. Build the Go SDK, the TypeScript SDK and the CLI again from the new OpenAPI document; in Go, give a defaulted member of a create request as a pointer, or leave it out to store the default. A script that reads the status `configured` of a repeated apply also reads `unchanged`. |
| 3.236.0 | 1.236.0 | 1.236.0 | Runtime v0.172.0 and postgres-adapter 6.21.0. The database instance of a seal checkpoint is now the database, the start of its server, and the table `stego_schema.seals`. So a restore in place, with `pg_restore --clean` and no `--create`, is a new instance too. In a new instance, as after a restore or a restart of the database server, the first run of the seal job ends the pass of the row, also a completed pass, and waits. The next run does a new pass. The contract step of a seal refuses with `the last pass of the seal job did not run in this database instance` until that pass completes. Before, a completed pass in a dump counted after the restore. A database server that restarts more often than each 10 minutes never gets a pass. The functions of the seal checkpoints are no longer in the body of a migration: each migration Job runs them again with `CREATE OR REPLACE`, so a database where the expand step of a seal ran with postgres-adapter 6.19.0 or 6.20.0 passes the ledger check and gets the new functions. Its ledger rows keep their digests, which this release accepts as earlier digests of the same steps. The first run of the seal job after the upgrade sees each open seal in a new database instance, so each open seal does one more pass. | After the upgrade, after a restore of a database with an open seal, and after a restart of the database server, run the release of the expand step with its controller until the seal job logs `seal of a secret column` with `completed=true` again, and only then run the migration Job of the contract step (see [Restore from a backup](operations.md#restore-from-a-backup)). Keep the database server up for two runs of the seal job, at most 20 minutes, and the time of the pass. You need not contract an open seal before the upgrade. Do not put the contract step of a seal in the first release that you build with postgres-adapter 6.21.0: first build the release of the expand step again with postgres-adapter 6.21.0 and runtime v0.172.0, deploy it, and let its seal job complete a pass. Then deploy the release of the contract step. Until then the contract refuses, and its refusal names this remedy. |
| 3.238.0 | 1.238.0 | 1.238.0 | cli-application 1.25.0. No kind loses `apply`. After a create that the service answers 409, because an entity holds `name` unique (`unique: true`, or a `unique_composite` of `name` and implicit fields), apply looks the resource up again and reports `unchanged`, or `failed` for other values or a resource that it cannot see. The CLI keeps the `Authz-Revocation-Location` of its last write in its configuration file and sends it in `Authz-Revocation-After` on each apply lookup, so an apply soon after a create finds the resource and makes no second one. It stops sending a kept value that is older than 10 minutes or whose lookup answer is no longer pending, and keeps nothing from a write after a login to another service. When the name is not unique, two clients that apply one new name at the same time can still make two resources; `apply --help` says so. When the patch takes `If-Match`, apply sends the generation that it read. A failed request gives the problem type and each entry in double quotes. Checking refuses a `default` on a field that a collection names in its `scope`. | To close the race of two clients on a new service, declare `unique: true` on the field `name` of the entity; on an entity of the base, no schema step makes a field unique yet. A script that reads the problem text of the CLI reads the quoted form. Remove the `default` of a scope field. |
| 3.240.0 | 1.240.0 | 1.240.0 | Runtime v0.175.0 and cli-application 1.26.0. `command.NewStruct(fields)` gives a command or worker fill the `google.protobuf.Struct` of a member of its message, with no import of a protobuf package, which the fill allow list refuses. It refuses objects and lists nested more than 64 levels. The generated CLI names the body member that it refuses before a request, such as `invalid request field "Title": the command takes no such member`; before, it said `invalid request field`. `stego fill sign <fill>` keeps a sign-off that already signs the current evidence with a key that `SIGNERS` lists, writes nothing and says so; `--force` signs such a fill again. | In a fill that must give a `Struct` member, call `command.NewStruct` (see [The imports of a fill](fills.md#the-imports-of-a-fill)). A script that reads the text of a refusal of the CLI reads the member name after `invalid request field`. A script that runs `stego fill sign` to sign again a fill whose sign-off is current, such as with another key, gives `--force`. |
| 3.241.0 | 1.241.0 | 1.241.0 | Runtime v0.176.0, command 1.24.0, worker 0.6.0 and rest-api 4.32.0: a worker can declare `reads`, a compare-only secret can have a limit of wrong attempts, and an entity can declare the fields of its change events. The change events of an entity with no `events` do not change. | None. A worker that only reads an entity can move it from `writes` to `reads`; its fill then uses `<Entity>Reader` (see [Who reads a secret](fills.md#who-reads-a-secret)). |
| 3.242.0 | 1.242.0 | 1.242.0 | rest-api 4.33.0 and postgres-adapter 6.22.0; no runtime change. A service whose history holds the expand step of a seal that postgres-adapter 6.14.0 to 6.20.0 wrote upgrades again: `stego plan --base`, `stego apply --base` and `stego compat` accept the earlier body of that step, with the functions of the seal checkpoints in it, because the release registers its digest as an earlier digest of the same migration. Before, they refused it with ST-GEN-INV38. Every other change of a released body is still refused. With no `idempotency` setting, a create or a patch with a malformed `Idempotency-Key` now gets 400 with the problem type `<problem base>idempotency-key-invalid`, and a key used with another body `idempotency-key-reused`, as a command does; before, the type was `about:blank` with the code `<SVC>-VAL-001`. A client that reads the type of these answers sees the new types. When the ledger of a database is not the history of the release, such as a shared preview database that ran the build of a branch, the migration Job logs a line that names the ledger row, the migration and the remedy. `stego test` now exits non-zero when it records no evidence for a fill, such as for a fill with no test file or a refused import, so a CI step of `stego test` fails then. | No change is needed: run step 3 with `--base <running tag>`, then the migration Job. A client that tells the faults of a key apart by the problem type reads the new types on a create and a patch. For a refused ledger, read the line of the Job and restore or make again the database that it names. When `stego test` fails with no evidence, correct each fill that its line names, then run `stego test` again. |
| 3.243.0 | 1.243.0 | 1.243.0 | Runtime v0.177.0 and rest-api 4.34.0. A collection create or patch reads the `Idempotency-Key` header before the body, as a command does: a key that is not one structured string of 1 to 255 characters gets 400 with the problem type `idempotency-key-invalid`, also when the body is not valid, and an absent key that the service requires `idempotency-key-missing`. Before, the body check answered first. `worker.run.failed` and `command.consumer.run_failed` give the cause `fill_json`, `fill_parse` or `fill_canceled` for a fill error of `encoding/json`, of `strconv` or `time`, or of a canceled context, read from its type and never its text; `fill_error` stays for another error. The failure record of a migration Job step that SpiceDB failed gives the cause class of its status code, such as `unavailable`. | A client that sends a malformed key and a body that is not valid now reads `idempotency-key-invalid` first. A dashboard or an alert that counts `worker.run.failed` with the cause `fill_error` also counts `fill_json`, `fill_parse` and `fill_canceled`. A fill that must tell an operator more logs its own context with `log/slog`, names and counts only (see [A failed run of a consuming worker](commands-queries-workers.md#a-failed-run-of-a-consuming-worker)). |
| 3.244.0 | 1.244.0 | 1.244.0 | Runtime v0.178.0 and postgres-adapter 6.23.0. Each process opens a small pool of its own, of at most 4 connections, for the charges of checks of a compare-only secret with `attempts`; a fill that holds the last connection of the main pool no longer makes its check wait 5 s and answer 503. A rerun of a command in the same request, such as after a conflict, does not count again the checks that an earlier attempt counted. `sealed.Compared.Stored` gives the error `sealed.ErrCounted` for a value of a field with `attempts` that a read gave, and no hash. The migration Job adds the columns and the default of `stego_schema.seals` only when they are not there, so it no longer takes an `ACCESS EXCLUSIVE` lock on that table at each run, and the check of the contract step of a seal puts the seal functions of the release in place in its own transaction. | Give the database at least one more connection for each process of the service, and at most 4 more under load, in `max_connections` or the connection limit of the pooler. A fill that calls `Stored` on such a value, such as to compare the hash itself, calls `Matches` instead. |
| 3.246.0 | 1.246.0 | 1.246.0 | Runtime v0.180.0. The runtime writes the value of a field with `attempts` that a read gave only into that field of that row; a fill that gives it to a compare-only field with no `attempts`, or to another row, gets an internal error (500) and the command commits nothing. A rerun of a command does not count again only the checks of the same stored value, so a new value that a reset wrote between two attempts gets its own counts. A charge of a check waits at most 1 s for a lock, and then the command answers 503. A full pool of the charges writes the warn line `command.attempts.pool_saturated`, with no value. | A fill that copies the value of a field with `attempts` to another field or row gives a new value with `sealed.ComparedOf` instead. Alert on `command.attempts.pool_saturated` if you want to know when the pool of the charges is too small. |
| each release | each release | - | A release that adds a subscription. | [Add a subscription to a running service](#add-a-subscription-to-a-running-service) |

## Move a browser-service database to the owner and migrator logins

Do this once, before the first migration Job of a browser service at
browser-service 1.149.0 or later.

Why: a release before browser-backend 4.6.0 used a LOGIN owner. That login
owned the database and the session table, and your controller or your
database administrator made the table with it. The migration Job of
browser-backend 4.6.0 and later checks a different layout:

- The owner role `<service>_owner` owns the database and the session table.
  It cannot log in, it is NOINHERIT, it has no other attribute, and it is a
  member of no role.
- The migration login `<service>_migrator` is a member of the owner role. The
  Job connects as it and takes the owner role.
- The runtime login `<service>_runtime` is NOINHERIT and a member of no role.
  It has CONNECT on the database, USAGE on the schema `public`, and SELECT,
  INSERT, UPDATE and DELETE on the session table.
- No other role, and not PUBLIC, has a grant on the database or on the schema
  `public`.

On the old layout, the Job stops with this failure record:

```json
{"severity":"ERROR","event.name":"service.failed","stage":"database.schema","fault":"owner","object":"public.stego_browser_sessions","remedy":"the database roles are not the layout of browser-backend 4.6.0 and later: ..."}
```

The fault `login` or `grant` with the same remedy also tells you to do these
steps. The move keeps the session table and its rows. The pods of the running
release connect as the runtime login, so they continue to serve during the
move.

Replace `<service>` with the service name, with each `-` as `_`. Do the steps
in this order:

1. As a database administrator, connected to any database of the server, run:

   ```sql
   -- As a database administrator, connected to any database of the server:
   ALTER ROLE <service>_owner NOLOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD NULL;
   ALTER ROLE <service>_runtime LOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
   CREATE ROLE <service>_migrator LOGIN IN ROLE <service>_owner;
   ALTER DATABASE <service> OWNER TO <service>_owner;
   REVOKE ALL ON DATABASE <service> FROM PUBLIC;
   GRANT CONNECT ON DATABASE <service> TO <service>_runtime;
   ```

2. Give the migration login a new secret in `psql`. `SHOW
   password_encryption` must give `scram-sha-256`. The `\password` command
   sends only the SCRAM verifier of the secret:

   ```psql
   SHOW password_encryption;
   \password <service>_migrator
   ```

3. As a database administrator, connected to the database `<service>`, run:

   ```sql
   -- As a database administrator, connected to the database <service>:
   ALTER TABLE public.stego_browser_sessions OWNER TO <service>_owner;
   ALTER SCHEMA public OWNER TO pg_database_owner;
   REVOKE ALL ON SCHEMA public FROM PUBLIC;
   GRANT USAGE ON SCHEMA public TO <service>_runtime;
   ```

4. Make sure that the layout has no other membership and no other grant. Each
   of these queries must give no row. Run them connected to the database
   `<service>`. For each row of the first three queries, run `REVOKE <role>
   FROM <member>`. For each row of the last query, run `REVOKE ALL ON DATABASE
   <service> FROM <grantee>`. The second and the third query are important
   after the move: a login that got the old LOGIN owner, such as a CI or DBA
   login, or a member of the migration login, can still take the owner role and
   read or drop the session table.

   The queries list only the memberships that give the privileges of the role
   (INHERIT or SET). On PostgreSQL 16 and later, an administrator that is not a
   superuser, such as the master user of a managed server (Amazon RDS, Cloud
   SQL, Azure), gets a membership WITH ADMIN OPTION only in each role that it
   makes. That membership cannot use the role, and the check accepts it. Do not
   revoke it: the administrator needs it to change the password of a login.

   ```sql
   -- The roles that the owner role and the runtime login are members of:
   SELECT g.rolname AS role, m.rolname AS member FROM pg_catalog.pg_auth_members a JOIN pg_catalog.pg_roles g ON g.oid = a.roleid JOIN pg_catalog.pg_roles m ON m.oid = a.member WHERE m.rolname IN ('<service>_owner', '<service>_runtime') AND (a.inherit_option OR a.set_option);
   -- The members of the owner role and the runtime login, other than the migration login:
   SELECT g.rolname AS role, m.rolname AS member FROM pg_catalog.pg_auth_members a JOIN pg_catalog.pg_roles g ON g.oid = a.roleid JOIN pg_catalog.pg_roles m ON m.oid = a.member WHERE g.rolname IN ('<service>_owner', '<service>_runtime') AND m.rolname <> '<service>_migrator' AND (a.inherit_option OR a.set_option);
   -- The members of the migration login, which can take the owner role through it:
   SELECT g.rolname AS role, m.rolname AS member FROM pg_catalog.pg_auth_members a JOIN pg_catalog.pg_roles g ON g.oid = a.roleid JOIN pg_catalog.pg_roles m ON m.oid = a.member WHERE g.rolname = '<service>_migrator' AND (a.inherit_option OR a.set_option);
   -- The grants on the database to a role that is not the owner role or the runtime login:
   SELECT a.grantee, a.privilege_type FROM pg_catalog.pg_database d CROSS JOIN pg_catalog.aclexplode(d.datacl) a WHERE d.datname = '<service>' AND a.grantee NOT IN (SELECT oid FROM pg_catalog.pg_roles WHERE rolname IN ('<service>_owner', '<service>_runtime'));
   ```

5. Change the key `url` of the Secret `<service>-database-owner` to the
   connection string of `<service>_migrator`, for example
   `postgres://<service>_migrator:<secret>@<host>:5432/<service>?sslmode=verify-full`.
   If your own controller made the session table with the old owner login,
   change it to use the migration login too.
6. Run the migration Job of the new release. It exits 0 when the layout is
   correct. The Job runs at each release, and a second run changes nothing.
7. Deploy the new release.

`PASSWORD NULL` removes the old password of the owner role, so a later
`ALTER ROLE ... LOGIN` cannot bring it back. The migration Job also refuses
an owner role with a member other than the migration login, a member of the
migration login, and a runtime login with a member, with the fault `owner` or
`login`. It counts only the memberships with INHERIT or SET, so an ADMIN-only
membership of the administrator is accepted.

When the Job still stops, its failure record names the fault class. See the
list of what the check needs in the README of the component
[browser-backend](../registry/components/browser-backend/README.md).

## Remove STEGO_MODULE

Why: `stego init` writes the module path into `go.mod`, and each later command
reads it from there. The variable `STEGO_MODULE` has no effect.

1. Make sure that `go.mod` of the project names the module path that your
   imports use.
2. Remove `STEGO_MODULE` from your shell profile, your CI file and your build
   scripts, so that nobody thinks that it sets the module.

## Make the Secret of the Kafka client

Why: kafka-producer 3.3.0 and kafka-consumer 0.3.0 give the SCRAM password
file and the CA file of the brokers a secret source. The worker Deployment
mounts the Secret `<service>-kafka-client` with the Component `secret-files`.
Without the Secret, the worker pod does not start.

Do this before you deploy the release, for each service with
`kafka-producer` or `kafka-consumer`:

1. Make the Secret `<service>-kafka-client` with two keys:
   - `password`: the SCRAM-SHA-512 password of the Kafka user of the service.
   - `ca-crt`: the PEM certificate authorities of the brokers.

   For example, with kubectl:

   ```sh
   kubectl create secret generic -n <namespace> <service>-kafka-client \
     --from-file=password=<file of the password> --from-file=ca-crt=<file of the CA>
   ```

   Use your secret operator in production, as for the other Secrets.
2. Set `STEGO_KAFKA_SASL_MECHANISM=SCRAM-SHA-512` and `STEGO_KAFKA_USERNAME` in
   the ConfigMap `<service>-config`, if they are not there.
3. Run `stego env check`. It refuses an environment file that does not give
   this Secret.

A cluster that uses mutual TLS or OAUTHBEARER in place of SCRAM does not set a
password. See [Secrets as files](delivery.md#secrets-as-files) for the patch
of the overlay.

## Give the worker ServiceAccount its grants

Why: the worker Deployment runs as the ServiceAccount `<service>-worker`. A
grant that you made outside the base for the ServiceAccount `<service>` does
not apply to it.

Before you apply the first release whose base holds
`deploy/base/worker-service-account.yaml` (rest-crud 3.176.0, rpc-service
1.178.0), give `<service>-worker` each grant
that the worker needs:

- a workload identity annotation, such as `eks.amazonaws.com/role-arn` or
  `iam.gke.io/gcp-service-account`, and the trust policy of that cloud role,
  which names the ServiceAccount;
- the `imagePullSecrets` of the ServiceAccount, when your overlay puts them
  there;
- the Vault Kubernetes auth role, whose `bound_service_account_names` must
  name `<service>-worker`;
- on OpenShift, an SCC that you gave to `<service>` with `oc adm policy
  add-scc-to-user`. Give the worker only the SCCs that it needs. Do not copy a
  broad SCC that only the API needs;
- each RoleBinding or ClusterRoleBinding that your platform made for
  `<service>`.

If you do not, the worker pods start without the grant: they cannot pull the
image, or they cannot get their cloud or Vault credentials. See [Probes and
modes](delivery.md#probes-and-modes).

## Write the tests of your fills

Why: an older `stego fill create` wrote a test that expects the unfinished
fill to return `ErrNotImplemented`. When you write the fill and remove
`ErrNotImplemented`, the test does not compile. An older `stego test` did not
run these tests, so you did not see this. A newer `stego test` runs them, and
it records no evidence for a fill whose tests fail. Then `stego plan`,
`stego apply` and `stego build` refuse the fill.

`stego test` names this cause for each such fill:

```text
fill register-technician: no evidence recorded: its tests failed: ...; fill_test.go still expects ErrNotImplemented, ...
```

For each fill that it names:

1. Open `fills/<fill>/fill_test.go`. Remove the test that expects
   `ErrNotImplemented`.
2. Write the tests of what the fill does. Give the fill a handle of a test,
   or call the functions that the fill uses with the values of the test.
3. Run `stego test`. Then sign the fill again with `stego fill sign <fill>`.

A new `stego fill create` writes a test that passes while the fill is not
written. When you write the fill, the test fails with a message that tells you
to replace it. It does not stop the compile.

## Seal the secret fields of a release before runtime v0.135.0

Do this when your running release is before runtime v0.135.0
(postgres-adapter 6.13.0) and its declaration has a field with `sensitivity:
secret`. Do it in the first release with runtime v0.137.0 or later.

Why: a release before runtime v0.135.0 stored a secret field in clear, in a
column of its type, such as `character varying(128)`. A newer release stores
it sealed, in a `bytea` column. The database holds the rows in clear, so the
newer release must move them with a seal step (decision ST-DECL-Q44). Without
it, `stego compat --base` and `stego plan --base` refuse the release with the
fault `the head stores <Entity>.<field> sealed, which the base stores in
clear`, and its suggested change is a seal `with from: secret`. The check reads
the version of postgres-adapter in `.stego/state.yaml` of the base.

1. If the running release is before runtime v0.120.0 (postgres-adapter
   6.4.0), first deploy a release with runtime v0.120.0 or later and no new
   schema step. Its processes report themselves, which the seal needs: `stego
   compat` refuses the seal over an older release.
2. Make the Secret `<service>-secret-key-ring` before you deploy. See [The key
   ring](operations.md#the-key-ring). `stego env check` names it when it is
   not there.
3. Change each fill that reads or writes the secret field. Its Go type is
   `sealed.Secret` (package `github.com/jsell-rh/stego/registry/runtime/sealed`):
   read its text with `v.ApiKey.Reveal()`, and make a value with
   `sealed.SecretOf(text)`. A `Secret` prints and logs as `[secret]` and
   encodes as JSON `null`, so a log line does not hold the value.
4. Declare a seal step with `from: secret` for each secret field, and keep the
   field as it is:

   ```yaml
   schema_changes:
     - {id: purse-api-key-seal, kind: seal, entity: Purse, field: api_key, from: secret}
   schema_history:
     - {change: purse-api-key-seal, phase: expand}
   ```

   The first migration `001_initial` stays the one of the running release.
   Run `stego compat --base <tag>` and `stego plan --base <tag>`: they accept
   the release.
5. Deploy the release. The migration Job adds the column `api_key_sealed`, and
   the release before goes on with the clear column while the Deployments
   roll. The new release writes both columns. The controller seals each row
   when no pod of the release before runs. See [Seal steps](schema-changes.md#seal-steps).
   You can go back to the release before in this phase: it reads the clear
   column, and the seal job checks each row again later. While a seal is open,
   do not go back past a release that reports itself (runtime v0.120.0 or
   later): an older release writes the clear column unseen.
6. In the next release, declare the contract step of each seal. Until then the
   clear column holds the secret, also in each backup that you take. Its migration
   refuses until the seal job completed a pass after the last pod of the
   release before stopped. Then it drops the clear column. After it, you cannot
   go back to a release before the expand step.

## Make the key ring of a service with a pii field

Do this before you deploy a release with runtime v0.138.0 or later, for a
service with a `pii` field and no `secret` field. A service with a `secret`
field has its key ring already.

Why: an answer of a command can hold a `pii` field, and the idempotency store
keeps each answer for the retention of its key, 24 hours by default. From
runtime v0.138.0 the store seals each answer with the key ring of the service
(ST-SUP-Q182 (b)). The api, worker and controller modes do not start without
it.

1. Make the Secret `<service>-secret-key-ring` with the key `key-ring`, as
   [The key ring](operations.md#the-key-ring) tells. Keep a copy of it in your
   secret store.
2. Run `stego env check`. It refuses an environment file that does not give
   this Secret.

A retry of an idempotency key that the release before stored, or that a
newer release stored when you go back, gets `idempotency-key-reused`; the
client sends it again with a new key.

## Remove the route of a stream query

Do this when `stego validate` gives the fault `query-stream-route`.

Why: a query whose `method` is a gRPC server stream is served over gRPC only
(ST-SUP-RC33-OC08). Its `route` was served by no handler: a request to it got
404 or 405. Checking now refuses the route, so the declaration says what the
service serves.

1. Remove the `route` of the stream query from `service.yaml`.
2. When an HTTP client must list the same rows, declare a second list query
   with a `route` and a unary `method`, or with no `method`, and its own fill.
   See [Declare a query](commands-queries-workers.md#a-get-query-and-a-list-query).
3. Run `stego validate` and `stego apply`.

## Use the new names of the query repositories

Do this in the first release at rest-crud 3.182.0 or rpc-service 1.184.0, or
in the release after it.

Why: the query contract named the repository of an entity with the plural
`<Entity>s`, such as `GetEntryEntrys` for the table `entries`. It now uses the
plural of the table names: `GetEntryEntries`, `ListBoxesBoxes`,
`ListCategoriesCategories`. The old name stays for one release as a function
marked `Deprecated` that calls the new one, so a fill still builds. Only an
entity whose name ends in `s`, `x`, `z`, `sh`, `ch`, or a consonant and `y`
has a new name.

1. In each query fill, replace each call of the old name with the new name.
   `staticcheck` (SA1019) and `gopls` name each call of a deprecated function.
2. Run `stego test` and `stego fill sign <fill>` for each fill that you
   changed.

## Give no parent where the repository takes none

Do this in the first release at rest-crud 3.206.0, rpc-service 1.206.0 or
browser-service 1.206.0.

Why: the runtime refused at run time each parent of a locked read, an update
that keeps its references and a delete of an entity with its own resource
type, and the command answered 500 (walkthrough 15). The generated repository
of such an entity now takes no parent for these calls, so the mistake fails
in `go build` and `stego test`. See [The repository of an
entity](fills.md#the-repository-of-an-entity).

1. Run `stego test`. Go names each call to change, such as `too many
   arguments in call to parcels.Update`.
2. Change each call that it names:
   - Remove the parent, or `nil`, from `GetForUpdate`, `GetForDelete`,
     `Delete` and an `Update` that keeps the references of the row.
   - Replace `Update` with `Move` for an update that changes a reference,
     and keep its parents: `parcels.Move(ctx, from, value, newShipment)`.
3. A fill that keeps the repository in a variable or a parameter of the type
   `*command.Repository[<Entity>]` uses the type `*commands.<Entity>Repo` or
   `*workers.<Entity>Repo` in its place.
4. Run `stego test` and `stego fill sign <fill>` for each fill that you
   changed.

A child entity, which has no resource type of its own, does not change: its
calls still take the loaded parent row.

## Add a subscription to a running service

A release that adds a subscription, a `kafka-consumer` with a consuming
worker, starts a new consumer group. The group reads the topic from its
first record that Kafka still holds. Thus the worker gets the backlog of the
topic at its first start.

The worker acts with its own service account. Its grant in each tenant is a
tuple that the controller writes. The controller of the new release writes
the tuples only when it holds the controller lease and its release is the
release of the last migration Job. In a rolling update, the pod of the release
before holds the lease until it stops. Thus the new worker can run before its
tuple is in SpiceDB.

- From runtime v0.134.0: the worker checks its tuple before each run. While
  the tuple is not there, the message stays pending with the failure code
  `worker-tuple-missing`, and the runtime does not count it toward poison.
  The backlog is applied when the controller of the new release has written
  the tuples. Do steps 1 to 3 below. Steps 4 to 7 are not necessary.
- Before runtime v0.134.0: each such run is refused, and after 5 refused runs
  in a row the worker parks the message. This takes a few seconds. A parked
  message holds back each later event of the same source row. No event is
  lost. Do all the steps below.

Do these steps:

1. Give the Kafka user of the service the ACLs of the subscription (see
   [Access to the topic](subscriptions.md#access-to-the-topic)).
2. Choose where the group starts. To apply the backlog, do nothing. To skip
   the backlog, set the offsets of the group to the end of the topic before the
   first start:

   ```sh
   kafka-consumer-groups.sh --bootstrap-server <broker> --command-config <client properties> \
     --group <service>.<subscription> --topic <source topic> --reset-offsets --to-latest --execute
   ```

3. Deploy the release.
4. Before runtime v0.134.0 only: wait until the worker applies a new event of the source, or 2 minutes after
   the last pod of the release before stopped. Then the controller of the new
   release holds the lease, and it wrote the tuples.
5. In each tenant, list the parked messages of the destination of the
   subscription. The destination has the name of the subscription. Do not put
   the token on the command line, where `ps` and the shell history show it.
   Write the header into a file that only you can read (curl 7.55 or later):

   ```sh
   (umask 077; printf 'Authorization: Bearer %s\n' "$(cat <file of the token>)" > auth-header)
   curl -sS -H @auth-header "https://<host><base path>/stego/outbox/<subscription>/parked"
   ```

6. Send each parked message again, then remove the header file:

   ```sh
   curl -sS -H @auth-header "https://<host><base path>/stego/outbox/<subscription>/parked" |
     jq -r '.items[].id' | while read -r id; do
       curl -sS -X POST -H @auth-header -H 'Content-Type: application/json' \
         -d '{"resolution": "not_acted"}' "https://<host><base path>/stego/outbox/<subscription>/parked/$id:resolve"
     done
   shred -u auth-header
   ```

   Do this again while the list has a `next` cursor. The caller needs the
   permission `resolve` on `outbox_destination:<subscription>` in the tenant
   (see [See and resolve the parked
   messages](commands-queries-workers.md#see-and-resolve-the-parked-messages)).
7. List the parked messages again. A message that parks again has another
   cause, such as a row that the fill needs and that does not exist. Correct
   the cause, then send it again.

From runtime v0.134.0, look at the parked list once after the first start
all the same: a message that parks has another cause, such as a row that the
fill needs and that does not exist.

## A Go build fails with "could not import log/slog"

The Go command keeps an index of each module version of the module cache in
its build cache (`go env GOCACHE`). It makes the index one time and uses it
again, because it takes the content of a module version as fixed. A released
version of the STEGO runtime library never changes its content. A runtime
version that you fetched before its release, such as from a branch of a pull
request, can change its content and its sum before the release. Take such a
new sum only from the go.sum line of the project that gives the version, and
never turn off the check of sums (do not set GONOSUMDB, GONOSUMCHECK,
GOSUMDB=off or GOFLAGS=-insecure). When the
module cache then holds the new content and the build cache holds the index
of the old content, a Go build can fail with an error such as:

```text
could not import log/slog (open : no such file or directory)
```

The same error can come from `go build`, `go test` or `go vet`, and from
`stego test` or `stego deps`, which run Go commands. The error names a
package of the standard library, but the cause is the old index. To correct
it, clear the build cache, then build again:

```sh
go clean -cache
```

To build one time without the index, and keep the build cache, set
`GODEBUG=goindex=0` for that command only, such as
`GODEBUG=goindex=0 go build ./...`. When the error comes back, clear the build
cache. A build cache that more users or builds share has the same old index
for each of them: clear it when no build runs.
