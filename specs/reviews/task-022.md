# Review: Task 022 — GORM-Based postgres-adapter

## Round 1

- [ ] **DAO method naming mismatch**: AC #3 requires methods named `Get` and `Replace` (matching the spec at rest-crud/spec.md:328-329: `Get(ctx, id)`, `Replace(ctx, entity)`). The implementation provides `Read` and `Update` instead (`generator.go:419`, `generator.go:439`). Rename `Read` to `Get` and `Update` to `Replace` to match the spec's DAO contract.

- [ ] **DAO methods missing `context.Context` parameter**: The spec defines every DAO method with `ctx` as the first parameter (rest-crud/spec.md:327-333: `Create(ctx, entity)`, `Get(ctx, id)`, `Replace(ctx, entity)`, `Delete(ctx, id)`, `List(ctx, listArgs)`, `Upsert(ctx, entity, upsertKey, concurrency)`, `Exists(ctx, id)`). The implementation omits `context.Context` from all seven Store methods. Context is required for GORM's `db.WithContext(ctx)` and for OpenTelemetry trace propagation (the archetype includes `otel-tracing`).

- [ ] **List method lacks pagination**: Both the spec (rest-crud/spec.md:331) and the task description (task-022.md:33) specify `List(ctx, listArgs) — g2.Offset(offset).Limit(limit).Find(&list) with pagination`. The implementation at `generator.go:501-539` does not support offset or limit — it calls `query.Find(&result)` without pagination parameters.

- [ ] **GORM DB connection never closed**: `writeGORMDBSetup` (`assembler.go:425-434`) does not close the database connection. The raw SQL equivalent (`writeDBSetup` at `assembler.go:413-423`) has `defer db.Close()`. The GORM version should retrieve the underlying `*sql.DB` via `db.DB()` and defer its close.

- [ ] **Migrations not wired to run at startup**: The spec states "GORM AutoMigrate at startup" (rest-crud/spec.md:337). The generator produces `Register`, `Migrate`, and `init()` registration infrastructure (`generator.go:654-717`), but nothing in the wiring or assembler calls `Migrate(db)` in the generated `main.go`. The migration infrastructure is generated but never invoked at runtime. The wiring (`generator.go:124-134`) should include a mechanism (e.g. a pre-constructor call or a new Wiring field) to ensure `storage.Migrate(db)` is called after DB setup and before constructors.
