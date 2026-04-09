# Task 022: GORM-Based postgres-adapter

**Spec Reference:** "Components > postgres-adapter" (rest-crud spec)

**Status:** `ready-for-review`

**Review:** [specs/reviews/task-022.md](../reviews/task-022.md)

## Description

The rest-crud archetype spec defines the postgres-adapter as GORM-based, following the rh-trex/hyperfleet-api pattern. The current implementation generates raw SQL. This task replaces the raw SQL generation with GORM-based code generation.

### What changes

**postgres-adapter generator (`internal/generator/postgresadapter/`):**

**Model structs:**
- All entity models embed a `Meta` base struct with `ID string`, `CreatedTime time.Time`, `UpdatedTime time.Time`, `DeletedAt gorm.DeletedAt`.
- GORM struct tags derived from entity field constraints:
  - `unique` → `gorm:"uniqueIndex"`
  - `max_length` → `gorm:"size:N"`
  - `optional` → nullable (no `not null`)
  - non-optional → `gorm:"not null"`
- `jsonb` fields use `gorm.io/datatypes.JSON` with `gorm:"type:jsonb"`.
- `ref` fields generate foreign key relationships.
- `computed` fields included in model but excluded from create/update inputs.
- Soft delete via `gorm.DeletedAt` with index.

**DAO layer:**
- Generate per-entity data access functions:
  - `Create(ctx, entity)` — `g2.Create(entity)`
  - `Get(ctx, id)` — `g2.First(&entity, id)`
  - `Replace(ctx, entity)` — `g2.Save(entity)` (for update and patch)
  - `Delete(ctx, id)` — soft delete via GORM
  - `List(ctx, listArgs)` — `g2.Offset(offset).Limit(limit).Find(&list)` with pagination
  - `Upsert(ctx, entity, upsertKey, concurrency)` — `g2.Clauses(clause.OnConflict{...})`
  - `Exists(ctx, id)` — existence check for parent verification

**GenericDao:**
- Base DAO type that `tsl-search` component can build queries on top of with ordering, filtering, JOINs, and pagination.

**Migrations:**
- GORM AutoMigrate at startup instead of raw SQL migrations.
- Generate migration structs registered in order:
  ```go
  func init() {
      Register("001_initial", func(g2 *gorm.DB) error {
          return g2.AutoMigrate(&Cluster{}, &NodePool{})
      })
  }
  ```

**SessionFactory:**
- Generate `SessionFactory` interface for database connection management.
- Support both production (PostgreSQL) and test (testcontainers) configurations.

**Wiring:**
- Generated go.mod includes GORM dependencies (`gorm.io/gorm`, `gorm.io/driver/postgres`, `gorm.io/datatypes`).
- Component version bumped to 2.0.0.

**Tests:**
- Update all postgres-adapter generator tests.
- Verify generated GORM code compiles with correct struct tags.

## Spec Excerpt

> Generates GORM-based model structs, DAO layer, and database migrations. Uses GORM as the ORM, following the proven rh-trex/hyperfleet-api pattern.
>
> ```go
> type Meta struct {
>     ID          string
>     CreatedTime time.Time
>     UpdatedTime time.Time
>     DeletedAt   gorm.DeletedAt `gorm:"index"`
> }
> ```

## Acceptance Criteria

1. Generated model structs embed `Meta` and use GORM struct tags.
2. `jsonb` fields use `datatypes.JSON`.
3. DAO layer provides Create, Get, Replace, Delete, List, Upsert, Exists.
4. Soft delete via `gorm.DeletedAt`.
5. Migrations use GORM AutoMigrate pattern.
6. SessionFactory interface generated.
7. GenericDao base type generated for search integration.
8. Generated go.mod includes GORM dependencies.
9. All generator tests pass.
10. `go build ./cmd/stego` compiles.

## Task Completion

When done, update this file's Status to `ready-for-review` and list relevant commits below.

## Commits

- 055716f feat(task-022): replace raw SQL postgres-adapter with GORM-based generation
- aa196e7 fix(task-022): address all 5 review findings on GORM-based postgres-adapter
- 797894c fix(task-022): address 3 round 2 review findings on GORM-based postgres-adapter
- cd4df58 fix(task-022): address 3 round 3 review findings on GORM-based postgres-adapter
- 6750a73 fix(task-022): address 2 round 4 review findings on GORM-based postgres-adapter
- 4234c05 fix(task-022): add COUNT(*) query and total count to Store.List for pagination
- c6b4afe fix(task-022): use actual item count for response envelope size field
