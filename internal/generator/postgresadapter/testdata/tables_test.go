package check_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"example.com/tables-check/boards"
	"example.com/tables-check/boardsbase"
	"example.com/tables-check/boardsnote"
	"example.com/tables-check/boardsrn"
	"example.com/tables-check/boardsrnbase"
	"example.com/tables-check/boardsslug"
	"example.com/tables-check/bounds"
	"example.com/tables-check/previous"
	"example.com/tables-check/storage"
	"example.com/tables-check/tenancy"
	"example.com/tables-check/widgets"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/jsell-rh/stego/registry/runtime/lease"
	"github.com/jsell-rh/stego/registry/runtime/manifest"
)

// server opens the PostgreSQL server of STEGO_TEST_POSTGRES_DSN as its administrator and drops the roles of the
// tenancy component when the test ends, because roles belong to the server and not to one database.
func server(t *testing.T) (*pgx.ConnConfig, *sql.DB) {
	t.Helper()
	dsn := os.Getenv("STEGO_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Fatal("STEGO_TEST_POSTGRES_DSN names no PostgreSQL server")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin := stdlib.OpenDB(*cfg)
	t.Cleanup(func() {
		for _, role := range []string{tenancy.RuntimeRole, tenancy.OwnerRole} {
			if _, err := admin.Exec("DROP ROLE IF EXISTS " + pgx.Identifier{role}.Sanitize()); err != nil {
				t.Error(err)
			}
		}
		admin.Close()
	})
	return cfg, admin
}

// database makes a new database with the roles of the tenancy component, and gives the administrator's
// connection to it. It is dropped before the roles.
func database(t *testing.T, cfg *pgx.ConnConfig, admin *sql.DB) (*pgx.ConnConfig, *sql.DB) {
	t.Helper()
	name := "tables_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec("CREATE DATABASE " + pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	own := cfg.Copy()
	own.Database = name
	db := stdlib.OpenDB(*own)
	t.Cleanup(func() {
		db.Close()
		// The drop gets 30 seconds, and a drop that fails is logged: the cleanup alone never fails the test.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := admin.ExecContext(ctx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Logf("pgtest: cleanup failed: drop the database %s: %v", name, err)
		}
	})
	if _, err := db.Exec(tenancy.Roles); err != nil {
		t.Fatalf("role migration: %v", err)
	}
	return own, db
}

// migrate runs the migration job of one release as the owner role, on one connection so that SET ROLE holds for
// every statement, and then grants the runtime role the access that the adapter declares in accessFile.
func migrate(t *testing.T, cfg *pgx.ConnConfig, admin *sql.DB, run func(context.Context, *sql.Conn) error, accessFile string) {
	t.Helper()
	ctx := context.Background()
	job := stdlib.OpenDB(*cfg)
	defer job.Close()
	conn, err := job.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "SET ROLE "+pgx.Identifier{tenancy.OwnerRole}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	if err := run(ctx, conn); err != nil {
		t.Fatalf("migrate as the owner role: %v", err)
	}
	data, err := os.ReadFile(accessFile)
	if err != nil {
		t.Fatal(err)
	}
	var objects []struct {
		Schema, Name, Kind string
		Privileges         []string
	}
	if err := json.Unmarshal(data, &objects); err != nil {
		t.Fatal(err)
	}
	for _, object := range objects {
		if len(object.Privileges) == 0 {
			continue
		}
		kind := "TABLE"
		if object.Kind == "sequence" {
			kind = "SEQUENCE"
		}
		statement := "GRANT " + strings.Join(object.Privileges, ", ") + " ON " + kind + " " + pgx.Identifier{object.Schema, object.Name}.Sanitize() + " TO " + pgx.Identifier{tenancy.RuntimeRole}.Sanitize()
		if _, err := admin.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if _, err := admin.Exec("GRANT USAGE ON SCHEMA stego_schema TO " + pgx.Identifier{tenancy.RuntimeRole}.Sanitize()); err != nil {
		t.Fatal(err)
	}
}

// catalogQueries read the schema of the service's two schemas from pg_catalog in a fixed order: the relations
// with their row-level security, owner and grants, the columns with their types, nullability and defaults by
// name, the constraints, the indexes, the triggers, the policies and the ledger rows without their apply time.
var catalogQueries = []string{
	`SELECT n.nspname, c.relname, c.relkind::text, c.relrowsecurity, c.relforcerowsecurity, pg_catalog.pg_get_userbyid(c.relowner), coalesce(c.relacl::text, '')
FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname IN ('public', 'stego_schema') ORDER BY 1, 2`,
	`SELECT c.relname, a.attname, pg_catalog.format_type(a.atttypid, a.atttypmod), a.attnotnull, coalesce(pg_catalog.pg_get_expr(d.adbin, d.adrelid), '')
FROM pg_catalog.pg_attribute a JOIN pg_catalog.pg_class c ON c.oid = a.attrelid JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_catalog.pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
WHERE n.nspname IN ('public', 'stego_schema') AND c.relkind IN ('r', 'p') AND a.attnum > 0 AND NOT a.attisdropped ORDER BY 1, 2`,
	`SELECT c.relname, k.conname, k.contype::text, pg_catalog.pg_get_constraintdef(k.oid)
FROM pg_catalog.pg_constraint k JOIN pg_catalog.pg_class c ON c.oid = k.conrelid JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname IN ('public', 'stego_schema') ORDER BY 1, 2`,
	`SELECT schemaname, tablename, indexname, indexdef FROM pg_catalog.pg_indexes WHERE schemaname IN ('public', 'stego_schema') ORDER BY 1, 2, 3`,
	`SELECT c.relname, t.tgname, pg_catalog.pg_get_triggerdef(t.oid)
FROM pg_catalog.pg_trigger t JOIN pg_catalog.pg_class c ON c.oid = t.tgrelid JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname IN ('public', 'stego_schema') AND NOT t.tgisinternal ORDER BY 1, 2`,
	`SELECT schemaname, tablename, policyname, permissive, roles::text, cmd, coalesce(qual, ''), coalesce(with_check, '')
FROM pg_catalog.pg_policies WHERE schemaname IN ('public', 'stego_schema') ORDER BY 1, 2, 3`,
	`SELECT version, digest FROM stego_schema.migrations ORDER BY version`,
}

// catalog reads the catalog of the database as lines of text, one for each row of catalogQueries.
func catalog(t *testing.T, db *sql.DB) []string {
	t.Helper()
	var lines []string
	for i, query := range catalogQueries {
		rows, err := db.Query(query)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for j := range values {
				pointers[j] = &values[j]
			}
			if err := rows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			parts := make([]string, len(values))
			for j, value := range values {
				parts[j] = fmt.Sprint(value)
			}
			lines = append(lines, fmt.Sprintf("%d|%s", i, strings.Join(parts, "|")))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
	}
	return lines
}

// column is one column of a table as pg_catalog states it.
type column struct {
	Name, Type, Default string
	NotNull             bool
}

// columns reads the columns of a table of public in their order in the table.
func columns(t *testing.T, db *sql.DB, table string) []column {
	t.Helper()
	rows, err := db.Query(`SELECT a.attname, pg_catalog.format_type(a.atttypid, a.atttypmod), a.attnotnull, coalesce(pg_catalog.pg_get_expr(d.adbin, d.adrelid), '')
FROM pg_catalog.pg_attribute a LEFT JOIN pg_catalog.pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
WHERE a.attrelid = $1::regclass AND a.attnum > 0 AND NOT a.attisdropped ORDER BY a.attnum`, "public."+table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var found []column
	for rows.Next() {
		var c column
		if err := rows.Scan(&c.Name, &c.Type, &c.NotNull, &c.Default); err != nil {
			t.Fatal(err)
		}
		found = append(found, c)
	}
	return found
}

// insertOrder writes one Order row as the runtime role in a transaction of the tenant, as a generated command
// does, and gives the version and the token the row got.
func insertOrder(t *testing.T, db *sql.DB, tenant, sku string) (int64, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("SET LOCAL ROLE " + pgx.Identifier{tenancy.RuntimeRole}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("SELECT pg_catalog.set_config('app.tenant', $1, true)", tenant); err != nil {
		t.Fatal(err)
	}
	var version int64
	var token string
	if err := tx.QueryRow(`INSERT INTO public."orders" ("id", "sku", "qty") VALUES ($1, $2, 3) RETURNING "version", "authz_token"`,
		uuid.Must(uuid.NewV7()).String(), sku).Scan(&version, &token); err != nil {
		t.Fatalf("insert an order as the runtime role: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return version, token
}

// write runs statement as the runtime role in a transaction of the tenant and gives the SQLSTATE of its error, or
// "" when it commits.
func write(t *testing.T, db *sql.DB, tenant, statement string, args ...any) string {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("SET LOCAL ROLE " + pgx.Identifier{tenancy.RuntimeRole}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("SELECT pg_catalog.set_config('app.tenant', $1, true)", tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(statement, args...); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			return pgErr.Code
		}
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return ""
}

// writeMarked runs one statement as write does, in a transaction of the release of a schema step, which sets the
// writer marker stego.schema_step to the open changes of the release first (ST-SUP-RC01-OC23).
func writeMarked(t *testing.T, db *sql.DB, tenant, marker, statement string, args ...any) string {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("SET LOCAL ROLE " + pgx.Identifier{tenancy.RuntimeRole}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("SELECT pg_catalog.set_config('app.tenant', $1, true), pg_catalog.set_config('stego.schema_step', $2, true)", tenant, marker); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(statement, args...); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			return pgErr.Code
		}
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return ""
}

// longField is a field whose check constraint name is longer than PostgreSQL keeps.
const longField = "a_long_field_name_that_pushes_the_check_name_past_the_limit"

func TestTables(t *testing.T) {
	cfg, admin := server(t)

	t.Run("ST-SUP-RC01-OC41-T01 The tables are SQL from the declaration, with no store", func(t *testing.T) {
		freshCfg, fresh := database(t, cfg, admin)
		migrate(t, freshCfg, fresh, func(ctx context.Context, conn *sql.Conn) error { return storage.Migrate(ctx, conn) }, "access.json")

		want := []column{
			{Name: "tenant_id", Type: "text", NotNull: true, Default: "current_setting('app.tenant'::text)"},
			{Name: "id", Type: "uuid", NotNull: true},
			{Name: "sku", Type: "text", NotNull: true},
			{Name: "qty", Type: "bigint", NotNull: true},
			{Name: "created_time", Type: "timestamp with time zone", NotNull: true, Default: "now()"},
			{Name: "updated_time", Type: "timestamp with time zone", NotNull: true, Default: "now()"},
			{Name: "version", Type: "bigint", NotNull: true, Default: "1"},
			{Name: "authz_token", Type: "text", NotNull: true, Default: "''::text"},
		}
		got := columns(t, fresh, "orders")
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("the table orders does not match the declaration:\n got %v\nwant %v", got, want)
		}
		rows, err := fresh.Query(`SELECT ic.relname, i.indisprimary, a.attname FROM pg_catalog.pg_index i
JOIN pg_catalog.pg_class ic ON ic.oid = i.indexrelid
JOIN pg_catalog.pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = i.indkey[0]
WHERE i.indrelid = 'public.orders'::regclass`)
		if err != nil {
			t.Fatal(err)
		}
		indexes, primary := 0, false
		for rows.Next() {
			var index, first string
			var isPrimary bool
			if err := rows.Scan(&index, &isPrimary, &first); err != nil {
				t.Fatal(err)
			}
			indexes++
			primary = primary || isPrimary
			if first != "tenant_id" {
				t.Errorf("the index %s of orders starts with %s, not with the tenant column", index, first)
			}
		}
		rows.Close()
		if !primary || indexes < 2 {
			t.Errorf("orders has %d indexes, the primary key %v; want the primary key and the unique index of sku", indexes, primary)
		}
		if version, token := insertOrder(t, fresh, "tenant-a", "sku-1"); version != 1 || token != "" {
			t.Errorf("a new row has the version %d and the token %q; want 1 and no token", version, token)
		}

		// The upgraded database: the migration job of the release before this one runs, the service writes a row,
		// and then the migration job of this release runs on the same database.
		upgradedCfg, upgraded := database(t, cfg, admin)
		migrate(t, upgradedCfg, upgraded, func(ctx context.Context, conn *sql.Conn) error { return previous.Migrate(ctx, conn) }, "access.json")
		insertOrder(t, upgraded, "tenant-a", "sku-1")
		migrate(t, upgradedCfg, upgraded, func(ctx context.Context, conn *sql.Conn) error { return storage.Migrate(ctx, conn) }, "access.json")
		var kept int
		if err := upgraded.QueryRow(`SELECT count(*) FROM public."orders"`).Scan(&kept); err != nil || kept != 1 {
			t.Errorf("the upgraded database keeps %d rows, error %v; want 1", kept, err)
		}
		freshCatalog, upgradedCatalog := catalog(t, fresh), catalog(t, upgraded)
		if strings.Join(freshCatalog, "\n") != strings.Join(upgradedCatalog, "\n") {
			t.Errorf("a new database and an upgraded one differ (ST-GEN-INV35):\nnew:\n%s\nupgraded:\n%s",
				strings.Join(freshCatalog, "\n"), strings.Join(upgradedCatalog, "\n"))
		}
		if len(freshCatalog) == 0 {
			t.Error("the catalog is empty")
		}
	})

	t.Run("ST-SUP-RC01-OC41-T02 Each field type and constraint is a column that PostgreSQL holds", func(t *testing.T) {
		cfg, db := database(t, cfg, admin)
		migrate(t, cfg, db, func(ctx context.Context, conn *sql.Conn) error { return widgets.Migrate(ctx, conn) }, "widgets_access.json")
		types := map[string]string{}
		for _, c := range columns(t, db, "widgets") {
			types[c.Name] = fmt.Sprint(c.Type, " ", c.NotNull)
		}
		want := map[string]string{"tenant_id": "text true", "id": "uuid true", "name": "character varying(64) true", "kind": "text true",
			"weight": "double precision true", "ratio": "real false", "count": "integer true", "total": "bigint true", "active": "boolean true",
			"payload": "jsonb false", "blob": "bytea false", "seen": "timestamp with time zone false", "code": "text true", "serial": "text true",
			"version": "bigint true", "authz_token": "text true", longField: "text false", "created_time": "timestamp with time zone true", "updated_time": "timestamp with time zone true"}
		if fmt.Sprint(types) != fmt.Sprint(want) {
			t.Errorf("the columns of widgets:\n got %v\nwant %v", types, want)
		}
		insert := `INSERT INTO public."widgets" ("id", "name", "kind", "weight", "count", "total", "code", "serial") VALUES ($1, $2, $3, $4, 1, 2, $5, $6)`
		first := uuid.Must(uuid.NewV7()).String()
		if code := write(t, db, "tenant-a", insert, first, "first", "small", 1.5, "c1", "s1"); code != "" {
			t.Fatalf("a valid widget is refused with %s", code)
		}
		refused := map[string][]any{
			"an enum value that is not declared": {uuid.NewString(), "second", "huge", 1.5, "c2", "s2"},
			"a name under its min_length":        {uuid.NewString(), "x", "small", 1.5, "c3", "s3"},
			"a weight over its max":              {uuid.NewString(), "third", "small", 1001.0, "c4", "s4"},
			"a weight that is not a number":      {uuid.NewString(), "fourth", "small", "NaN", "c5", "s5"},
		}
		for name, args := range refused {
			if code := write(t, db, "tenant-a", insert, args...); code != "23514" {
				t.Errorf("%s: SQLSTATE %q, want the check violation 23514", name, code)
			}
		}
		if code := write(t, db, "tenant-a", insert, uuid.NewString(), "first", "large", 1.5, "c6", "s6"); code != "23505" {
			t.Errorf("a second widget named first of one tenant: SQLSTATE %q, want the unique violation 23505", code)
		}
		if code := write(t, db, "tenant-a", insert, uuid.NewString(), "fifth", "small", 1.5, "c1", "s7"); code != "23505" {
			t.Errorf("a second widget of the unique_composite code c1 and kind small: SQLSTATE %q, want 23505", code)
		}
		if code := write(t, db, "tenant-b", insert, uuid.NewString(), "first", "small", 1.5, "c1", "s1"); code != "" {
			t.Errorf("the name first of another tenant is refused with %s: the tenant column does not lead the unique index", code)
		}
		catalogID := uuid.NewString()
		if _, err := db.Exec(`INSERT INTO public."catalogs" ("id", "code") VALUES ($1, 'shared')`, catalogID); err != nil {
			t.Fatal(err)
		}
		order := `INSERT INTO public."orders" ("id", "widget_id", "catalog_id") VALUES ($1, $2, $3)`
		if code := write(t, db, "tenant-a", order, uuid.NewString(), first, catalogID); code != "" {
			t.Errorf("an order of the tenant's own widget is refused with %s", code)
		}
		if code := write(t, db, "tenant-b", order, uuid.NewString(), first, catalogID); code != "23503" {
			t.Errorf("an order of tenant-b that names a widget of tenant-a: SQLSTATE %q, want the foreign key violation 23503", code)
		}
	})
	t.Run("ST-SUP-RC01-OC12-T09 The schema manifest is the model of the migrations, which PostgreSQL holds", func(t *testing.T) {
		cfg, db := database(t, cfg, admin)
		migrate(t, cfg, db, func(ctx context.Context, conn *sql.Conn) error { return widgets.Migrate(ctx, conn) }, "widgets_access.json")
		// The runtime role reads the catalog, as the service runtime does at startup.
		runtime := cfg.Copy()
		runtime.User, runtime.Password = tenancy.RuntimeRole, "tables-check"
		if _, err := db.Exec("ALTER ROLE " + pgx.Identifier{tenancy.RuntimeRole}.Sanitize() + " LOGIN PASSWORD 'tables-check'"); err != nil {
			t.Fatal(err)
		}
		reader := stdlib.OpenDB(*runtime)
		defer reader.Close()
		ctx := context.Background()
		if err := manifest.Check(ctx, reader, owned(widgets.SchemaManifest)); err != nil {
			t.Fatalf("the database that the migrations made does not hold the schema manifest: %v", err)
		}
		// The bounds of each shape: each CHECK definition of the manifest is the text that PostgreSQL writes.
		boundsCfg, boundsDB := database(t, cfg, admin)
		migrate(t, boundsCfg, boundsDB, func(ctx context.Context, conn *sql.Conn) error { return bounds.Migrate(ctx, conn) }, "bounds_access.json")
		if _, err := boundsDB.Exec("ALTER ROLE " + pgx.Identifier{tenancy.RuntimeRole}.Sanitize() + " LOGIN PASSWORD 'tables-check'"); err != nil {
			t.Fatal(err)
		}
		boundsRuntime := boundsCfg.Copy()
		boundsRuntime.User, boundsRuntime.Password = tenancy.RuntimeRole, "tables-check"
		boundsReader := stdlib.OpenDB(*boundsRuntime)
		defer boundsReader.Close()
		if err := manifest.Check(ctx, boundsReader, owned(bounds.SchemaManifest)); err != nil {
			t.Fatalf("the database of the bounds does not hold the schema manifest: %v", err)
		}
		if _, err := db.Exec(`ALTER TABLE public."orders" DROP CONSTRAINT "orders_widget_id_fkey"`); err != nil {
			t.Fatal(err)
		}
		if err := manifest.Check(ctx, reader, owned(widgets.SchemaManifest)); !errors.Is(err, manifest.ErrNotSatisfied) ||
			!strings.Contains(err.Error(), "table public.orders: constraint orders_widget_id_fkey is missing") {
			t.Fatalf("a dropped foreign key gave %v, want the fault that names it", err)
		}
	})

	// renamed migrates the release before the rename, writes the board first with the title "first" as that
	// release, and migrates the rename at expand; it gives the database and the ID of the board.
	renamed := func(t *testing.T) (*pgx.ConnConfig, *sql.DB, string) {
		t.Helper()
		upCfg, up := database(t, cfg, admin)
		migrate(t, upCfg, up, func(ctx context.Context, conn *sql.Conn) error { return boardsrnbase.Migrate(ctx, conn) }, "boardsrnbase_access.json")
		first := uuid.Must(uuid.NewV7()).String()
		if code := write(t, up, "tenant-a", `INSERT INTO public."boards" ("id", "title") VALUES ($1, 'first')`, first); code != "" {
			t.Fatalf("the release before writes no board: %s", code)
		}
		migrate(t, upCfg, up, func(ctx context.Context, conn *sql.Conn) error { return boardsrn.Migrate(ctx, conn) }, "boardsrn_access.json")
		return upCfg, up, first
	}
	// pair reads the old and the new column of the board id.
	pair := func(t *testing.T, db *sql.DB, id string) (sql.NullString, sql.NullString) {
		t.Helper()
		var title, heading sql.NullString
		if err := db.QueryRow(`SELECT "title", "heading" FROM public."boards" WHERE "id" = $1`, id).Scan(&title, &heading); err != nil {
			t.Fatal(err)
		}
		return title, heading
	}
	// backfill moves the rows of tenant-a whose new column is unset as the backfill batch does, as the owner role with
	// the backfill setting of the change, and gives the SQLSTATE of the validation of the completeness CHECK of the
	// generated copy that follows it, or empty when it passes.
	backfill := func(t *testing.T, db *sql.DB, move bool) string {
		t.Helper()
		copy := boardsrn.BackfillCopies[0]
		if move {
			tx, err := db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			for _, statement := range []string{"SET LOCAL ROLE " + pgx.Identifier{tenancy.OwnerRole}.Sanitize(),
				"SELECT pg_catalog.set_config('stego.backfill_change', '" + copy.Change + "', true), pg_catalog.set_config('app.tenant', 'tenant-a', true)",
				`UPDATE public."boards" SET "heading" = "title" WHERE "heading" IS NULL`} {
				if _, err := tx.Exec(statement); err != nil {
					t.Fatalf("%.60s: %v", statement, err)
				}
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := db.Exec(copy.Validate()); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) {
				return pgErr.Code
			}
			t.Fatal(err)
		}
		return ""
	}
	// next is a write of the release of the rename: its transactions set the writer marker to its open changes.
	next := func(t *testing.T, db *sql.DB, statement string, args ...any) string {
		t.Helper()
		return writeMarked(t, db, "tenant-a", strings.Join(boardsrn.SchemaSteps, ","), statement, args...)
	}

	t.Run("ST-GEN-UC12-R02-T02 The expand migration of a rename keeps the old column and adds the new one nullable, with its CHECK, its indexes and the sync trigger", func(t *testing.T) {
		_, up, _ := renamed(t)
		var notNull, newNotNull bool
		var newDefault sql.NullString
		if err := up.QueryRow(`SELECT a.attnotnull FROM pg_catalog.pg_attribute a WHERE a.attrelid = 'public.boards'::regclass AND a.attname = 'title'`).Scan(&notNull); err != nil || !notNull {
			t.Errorf("the old column title is not NOT NULL any more (%v)", err)
		}
		if err := up.QueryRow(`SELECT a.attnotnull, pg_catalog.pg_get_expr(d.adbin, d.adrelid) FROM pg_catalog.pg_attribute a LEFT JOIN pg_catalog.pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
WHERE a.attrelid = 'public.boards'::regclass AND a.attname = 'heading'`).Scan(&newNotNull, &newDefault); err != nil || newNotNull || newDefault.Valid {
			t.Errorf("the new column heading is NOT NULL %v with the default %v (error %v), want nullable with no default", newNotNull, newDefault, err)
		}
		var indexes []string
		rows, err := up.Query(`SELECT indexname FROM pg_catalog.pg_indexes WHERE schemaname = 'public' AND tablename = 'boards' ORDER BY 1`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatal(err)
			}
			indexes = append(indexes, name)
		}
		rows.Close()
		for _, column := range []string{"title", "heading"} {
			if !slices.ContainsFunc(indexes, func(n string) bool { return strings.Contains(n, column) }) {
				t.Errorf("the table has the indexes %v, with no search index of %s", indexes, column)
			}
		}
		// title is unique: both columns have their unique key, valid, while both releases run (decision on ST-GEN-Q18 (a)).
		var keys int
		if err := up.QueryRow(`SELECT count(*) FROM pg_catalog.pg_index i JOIN pg_catalog.pg_class c ON c.oid = i.indexrelid
WHERE i.indrelid = 'public.boards'::regclass AND i.indisunique AND i.indisvalid AND c.relname IN ('boards_title_key', 'boards_heading_key')`).Scan(&keys); err != nil || keys != 2 {
			t.Errorf("the table has %d valid unique keys of title and heading (error %v), want 2", keys, err)
		}
		var checks int
		if err := up.QueryRow(`SELECT count(*) FROM pg_catalog.pg_constraint WHERE conrelid = 'public.boards'::regclass AND contype = 'c' AND convalidated AND pg_catalog.pg_get_constraintdef(oid) LIKE '%heading%'`).Scan(&checks); err != nil || checks != 1 {
			t.Errorf("the new column has %d validated CHECK constraints (error %v), want 1", checks, err)
		}
		var triggers int
		if err := up.QueryRow(`SELECT count(*) FROM pg_catalog.pg_trigger WHERE tgrelid = 'public.boards'::regclass AND NOT tgisinternal AND tgname = 'boards_heading_sync'`).Scan(&triggers); err != nil || triggers != 1 {
			t.Errorf("the table has %d sync triggers boards_heading_sync (error %v), want 1", triggers, err)
		}
	})

	t.Run("ST-SUP-RC01-OC23-T01 The sync trigger copies from the column that changed and refuses only two changes that differ", func(t *testing.T) {
		_, up, first := renamed(t)
		if title, heading := pair(t, up, first); title.String != "first" || heading.Valid {
			t.Fatalf("before the backfill the board of the release before is %v, %v; want first and no new value", title, heading)
		}
		before, after := uuid.Must(uuid.NewV7()).String(), uuid.Must(uuid.NewV7()).String()
		// The release before writes only the old column; the trigger copies it into the new one.
		if code := write(t, up, "tenant-a", `INSERT INTO public."boards" ("id", "title") VALUES ($1, 'old-only')`, before); code != "" {
			t.Fatalf("an insert of the release before: %s", code)
		}
		if title, heading := pair(t, up, before); title.String != "old-only" || heading.String != "old-only" {
			t.Errorf("an insert of the release before gives %v, %v; want old-only in both", title, heading)
		}
		// The release of the rename writes only the new column; the trigger copies it into the old one.
		if code := next(t, up, `INSERT INTO public."boards" ("id", "heading") VALUES ($1, 'new-only')`, after); code != "" {
			t.Fatalf("an insert of the release of the rename: %s", code)
		}
		if title, heading := pair(t, up, after); title.String != "new-only" || heading.String != "new-only" {
			t.Errorf("an insert of the release of the rename gives %v, %v; want new-only in both", title, heading)
		}
		both := uuid.Must(uuid.NewV7()).String()
		if code := next(t, up, `INSERT INTO public."boards" ("id", "title", "heading") VALUES ($1, 'same', 'same')`, both); code != "" {
			t.Errorf("an insert with both columns equal: %s", code)
		}
		if code := write(t, up, "tenant-a", `UPDATE public."boards" SET "title" = 'old-changed' WHERE "id" = $1`, after); code != "" {
			t.Fatalf("an update of the old column: %s", code)
		}
		if title, heading := pair(t, up, after); title.String != "old-changed" || heading.String != "old-changed" {
			t.Errorf("an update of the old column gives %v, %v; want old-changed in both", title, heading)
		}
		if code := next(t, up, `UPDATE public."boards" SET "heading" = 'new-changed' WHERE "id" = $1`, before); code != "" {
			t.Fatalf("an update of the new column: %s", code)
		}
		if title, heading := pair(t, up, before); title.String != "new-changed" || heading.String != "new-changed" {
			t.Errorf("an update of the new column gives %v, %v; want new-changed in both", title, heading)
		}
		if code := write(t, up, "tenant-a", `UPDATE public."boards" SET "title" = 'agree', "heading" = 'agree' WHERE "id" = $1`, before); code != "" {
			t.Errorf("an update of both columns to values that agree: %s", code)
		}
		for _, writer := range []func(*testing.T, *sql.DB, string, ...any) string{
			func(t *testing.T, db *sql.DB, s string, args ...any) string {
				return write(t, db, "tenant-a", s, args...)
			}, next,
		} {
			if code := writer(t, up, `UPDATE public."boards" SET "title" = 'one', "heading" = 'two' WHERE "id" = $1`, before); code != "23514" {
				t.Errorf("an update of both columns to values that differ gives %q, want 23514", code)
			}
		}
		if code := write(t, up, "tenant-a", `UPDATE public."boards" SET "note" = 'a note' WHERE "id" = $1`, before); code != "" {
			t.Fatalf("an update of a third column: %s", code)
		}
		if title, heading := pair(t, up, before); title.String != "agree" || heading.String != "agree" {
			t.Errorf("an update of a third column changed the pair to %v, %v", title, heading)
		}
	})

	t.Run("ST-SUP-RC01-OC25-T01 The checkpoint row is the owner's and changes only by the fenced statement", func(t *testing.T) {
		_, up, _ := renamed(t)
		var owner string
		var moved, token int64
		if err := up.QueryRow(`SELECT pg_catalog.pg_get_userbyid(c.relowner), b.rows_moved, b.token FROM stego_schema.backfills b, pg_catalog.pg_class c
WHERE c.oid = 'stego_schema.backfills'::regclass AND b.change_id = 'board-title-rename'`).Scan(&owner, &moved, &token); err != nil || owner != tenancy.OwnerRole || moved != 0 || token != 0 {
			t.Fatalf("the checkpoint row is owned by %q with %d moved and the token %d (error %v); want the owner role, 0 and 0", owner, moved, token, err)
		}
		for _, statement := range []string{
			`UPDATE stego_schema.backfills SET rows_moved = 99 WHERE change_id = 'board-title-rename'`,
			`DELETE FROM stego_schema.backfills WHERE change_id = 'board-title-rename'`,
			`INSERT INTO stego_schema.backfills (change_id) VALUES ('other')`,
		} {
			if code := write(t, up, "tenant-a", statement); code != "42501" {
				t.Errorf("as the runtime role %q gives %q, want 42501", statement, code)
			}
		}
		// The definer functions run with their own search path: a function of the caller's schema does not stand in
		// for one that they call.
		var definer bool
		var config string
		if err := up.QueryRow(`SELECT bool_and(prosecdef), string_agg(coalesce(array_to_string(proconfig, ';'), 'none'), '|' ORDER BY proname) FROM pg_catalog.pg_proc
WHERE pronamespace = 'stego_schema'::regnamespace AND proname IN ('backfill_read', 'backfill_checkpoint')`).Scan(&definer, &config); err != nil || !definer ||
			config != "search_path=pg_catalog, pg_temp|search_path=pg_catalog, pg_temp" {
			t.Errorf("the checkpoint functions are SECURITY DEFINER %v with %q (error %v), want true and search_path=pg_catalog, pg_temp", definer, config, err)
		}
		// EXECUTE is the owner's alone: PUBLIC and the runtime role cannot run either function, so the runtime role
		// cannot read, lock or change the row (decision on finding M1 of the security review of PR #291).
		checkpointFunction := "stego_schema.backfill_checkpoint(text, bytea, bigint, bigint, bigint, text, boolean, bigint, text, text, text)"
		for _, role := range []string{"public", tenancy.RuntimeRole} {
			var may bool
			if err := up.QueryRow(`SELECT has_function_privilege($1, 'stego_schema.backfill_read(text)', 'EXECUTE') OR has_function_privilege($1, $2, 'EXECUTE')`, role, checkpointFunction).Scan(&may); err != nil || may {
				t.Errorf("%s may run a checkpoint function: %v (error %v)", role, may, err)
			}
		}
		leaseName := "stego.backfill.board-title-rename"
		fenced := `SELECT stego_schema.backfill_checkpoint('board-title-rename', '\x01'::bytea, 5, 0, 0, '', false, $1, 'uuid', '` + leaseName + `', 'job-a')`
		for _, statement := range []string{
			`SELECT count(*) FROM stego_schema.backfill_read('board-title-rename')`,
			strings.Replace(fenced, "$1", "7", 1),
		} {
			if code := write(t, up, "tenant-a", statement); code != "42501" {
				t.Errorf("as the runtime role %q gives %q, want 42501", statement, code)
			}
		}
		// asOwner runs the statements in one transaction as the owner role, the role of the backfill in the migration
		// job, and gives the result of the last.
		asOwner := func(statements ...string) int64 {
			t.Helper()
			tx, err := up.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err := tx.Exec("SET LOCAL ROLE " + pgx.Identifier{tenancy.OwnerRole}.Sanitize()); err != nil {
				t.Fatal(err)
			}
			var changed int64
			for i, statement := range statements {
				if i < len(statements)-1 {
					if _, err := tx.Exec(statement); err != nil {
						t.Fatalf("%.60s: %v", statement, err)
					}
					continue
				}
				if err := tx.QueryRow(statement).Scan(&changed); err != nil {
					t.Fatalf("the fenced checkpoint statement as the owner role: %v", err)
				}
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			return changed
		}
		asOwner(lease.LeasesTable, `INSERT INTO public.leases (name, holder, expires_at, token) VALUES ('`+leaseName+`', 'job-a', now() + interval '1 hour', 7) ON CONFLICT (name) DO NOTHING`, `SELECT 0`)
		if got := asOwner(strings.Replace(fenced, "$1", "7", 1)); got != 1 {
			t.Errorf("the fenced statement of the lease holder with the token 7 changed %d rows, want 1", got)
		}
		if got := asOwner(strings.Replace(fenced, "$1", "3", 1)); got != 0 {
			t.Errorf("the fenced statement with the stale token 3 changed %d rows, want 0", got)
		}
		if err := up.QueryRow(`SELECT rows_moved, token FROM stego_schema.backfills WHERE change_id = 'board-title-rename'`).Scan(&moved, &token); err != nil || moved != 5 || token != 7 {
			t.Errorf("after the fenced statements the row has %d moved and the token %d (error %v), want 5 and 7", moved, token, err)
		}
	})

	t.Run("ST-SUP-RC01-OC23-T05 The completeness CHECK comes NOT VALID with the sync trigger, the release before restarts after each phase, and the release of the rename only after the validation", func(t *testing.T) {
		ctx := context.Background()
		upCfg, up, first := renamed(t)
		if _, err := up.Exec("ALTER ROLE " + pgx.Identifier{tenancy.RuntimeRole}.Sanitize() + " LOGIN PASSWORD 'tables-check'"); err != nil {
			t.Fatal(err)
		}
		runtime := upCfg.Copy()
		runtime.User, runtime.Password = tenancy.RuntimeRole, "tables-check"
		reader := stdlib.OpenDB(*runtime)
		defer reader.Close()
		copy := boardsrn.BackfillCopies[0]
		var definition string
		var validated bool
		read := func() {
			t.Helper()
			if err := up.QueryRow(`SELECT pg_catalog.pg_get_constraintdef(oid), convalidated FROM pg_catalog.pg_constraint WHERE conrelid = 'public.boards'::regclass AND conname = $1`, copy.Check).Scan(&definition, &validated); err != nil {
				t.Fatalf("the completeness CHECK %s: %v", copy.Check, err)
			}
		}
		read()
		if want := "CHECK (((heading IS NOT NULL) OR (title IS NULL))) NOT VALID"; definition != want || validated {
			t.Fatalf("after the expand migrations the CHECK is %q, validated %v; want %q", definition, validated, want)
		}
		accepts := func(phase string, wantRename bool) {
			t.Helper()
			if err := manifest.Check(ctx, reader, owned(boardsrnbase.SchemaManifest)); err != nil {
				t.Errorf("%s: the schema manifest of the release before refuses the database: %v", phase, err)
			}
			err := manifest.Check(ctx, reader, owned(boardsrn.SchemaManifest))
			if wantRename && err != nil {
				t.Errorf("%s: the schema manifest of the release of the rename refuses the database: %v", phase, err)
			}
			if !wantRename && err == nil {
				t.Errorf("%s: the schema manifest of the release of the rename accepts the completeness CHECK NOT VALID", phase)
			}
		}
		accepts("at expand", false)
		// The row that the release before wrote before the step has no new value: the validation fails, and the
		// CHECK stays NOT VALID.
		if code := backfill(t, up, false); code != "23514" {
			t.Fatalf("the validation with an unmoved row gives %q, want 23514", code)
		}
		read()
		if validated {
			t.Fatal("the CHECK is validated after a failed validation")
		}
		accepts("after a failed validation", false)
		// A write of the release before to another column of the unmoved row meets the CHECK: the trigger of
		// version 2 sets the new column.
		if code := write(t, up, "tenant-a", `UPDATE public."boards" SET "note" = 'a note' WHERE "id" = $1`, first); code != "" {
			t.Fatalf("an update of a third column of an unmoved row by the release before: %s", code)
		}
		if title, heading := pair(t, up, first); heading.String != title.String {
			t.Fatalf("after an update of a third column the pair is %v, %v; want the new column set from the old", title, heading)
		}
		// An insert of the release before and one of the release of the rename each meet the CHECK.
		if code := write(t, up, "tenant-a", `INSERT INTO public."boards" ("id", "title") VALUES ($1, 'old-only')`, uuid.Must(uuid.NewV7()).String()); code != "" {
			t.Fatalf("an insert of the release before: %s", code)
		}
		if code := next(t, up, `INSERT INTO public."boards" ("id", "heading") VALUES ($1, 'new-only')`, uuid.Must(uuid.NewV7()).String()); code != "" {
			t.Fatalf("an insert of the release of the rename: %s", code)
		}
		if code := backfill(t, up, true); code != "" {
			t.Fatalf("the validation after the backfill gives %s", code)
		}
		read()
		if !validated {
			t.Fatal("the CHECK is not validated after the validation passed")
		}
		accepts("after the backfill", true)
	})

	t.Run("ST-GEN-UC12-R03-T02 Each release accepts the schema of a rename at expand by its own manifest, and the release before refuses a changed sync function", func(t *testing.T) {
		ctx := context.Background()
		upCfg, up, _ := renamed(t)
		if _, err := up.Exec("ALTER ROLE " + pgx.Identifier{tenancy.RuntimeRole}.Sanitize() + " LOGIN PASSWORD 'tables-check'"); err != nil {
			t.Fatal(err)
		}
		runtime := upCfg.Copy()
		runtime.User, runtime.Password = tenancy.RuntimeRole, "tables-check"
		reader := stdlib.OpenDB(*runtime)
		defer reader.Close()
		if err := boardsrnbase.CheckLedger(ctx, up); err != nil {
			t.Errorf("the ledger check of the release before refuses the rename: %v", err)
		}
		if err := manifest.Check(ctx, reader, owned(boardsrnbase.SchemaManifest)); err != nil {
			t.Errorf("the schema manifest of the release before refuses the rename at expand: %v", err)
		}
		// The release of the rename runs after its backfill, which validates the completeness CHECK (decision on
		// ST-SUP-Q120 (c)).
		if code := backfill(t, up, true); code != "" {
			t.Fatalf("the validation of the completeness CHECK after the backfill gives %s", code)
		}
		if err := manifest.Check(ctx, reader, owned(boardsrnbase.SchemaManifest)); err != nil {
			t.Errorf("the schema manifest of the release before refuses the rename after its backfill: %v", err)
		}
		if err := manifest.Check(ctx, reader, owned(boardsrn.SchemaManifest)); err != nil {
			t.Errorf("the schema manifest of the release of the rename refuses its own expand step after its backfill: %v", err)
		}
		// A sync function that is not the template's text is refused by both.
		if _, err := up.Exec(`CREATE OR REPLACE FUNCTION stego_steps.boards_heading_sync() RETURNS trigger LANGUAGE plpgsql SECURITY INVOKER VOLATILE SET search_path = pg_catalog, pg_temp AS $f$BEGIN RETURN NEW; END;$f$`); err != nil {
			t.Fatal(err)
		}
		for name, m := range map[string]manifest.Manifest{"before": boardsrnbase.SchemaManifest, "rename": boardsrn.SchemaManifest} {
			if err := manifest.Check(ctx, reader, owned(m)); err == nil {
				t.Errorf("the schema manifest of the release %s accepts a sync function that is not the template's", name)
			}
		}
	})

	t.Run("ST-SUP-RC01-OC22-T03 An add step takes a database with rows to the schema of a new database, and the release before still runs on it", func(t *testing.T) {
		ctx := context.Background()
		upCfg, up := database(t, cfg, admin)
		migrate(t, upCfg, up, func(ctx context.Context, conn *sql.Conn) error { return boardsbase.Migrate(ctx, conn) }, "boardsbase_access.json")
		first := uuid.Must(uuid.NewV7()).String()
		if code := write(t, up, "tenant-a", `INSERT INTO public."boards" ("id", "title") VALUES ($1, 'first')`, first); code != "" {
			t.Fatalf("the release before writes no board: %s", code)
		}
		migrate(t, upCfg, up, func(ctx context.Context, conn *sql.Conn) error { return boards.Migrate(ctx, conn) }, "boards_access.json")

		// The row of the release before is kept, with no note and the default rank.
		var title string
		var note sql.NullString
		var rank int
		if err := up.QueryRow(`SELECT "title", "note", "rank" FROM public."boards" WHERE "id" = $1`, first).Scan(&title, &note, &rank); err != nil || title != "first" || note.Valid || rank != 0 {
			t.Errorf("the board of the release before is %q, %v, %d (error %v); want first, no note and rank 0", title, note, rank, err)
		}
		// Each step row has the empty compatible_with set of a step at expand, and the rows before it have none
		// (ST-SUP-RC01-OC27).
		rows, err := up.Query(`SELECT version, coalesce(compatible_with::text, 'none') FROM stego_schema.migrations ORDER BY version COLLATE "C"`)
		if err != nil {
			t.Fatal(err)
		}
		var ledger []string
		for rows.Next() {
			var version, set string
			if err := rows.Scan(&version, &set); err != nil {
				t.Fatal(err)
			}
			ledger = append(ledger, version+" "+set)
		}
		rows.Close()
		want := "001_initial none,100_tenancy_row_security none,step_0001_add-card_expand {},step_0002_add-board-note_expand {}," +
			"step_0002_add-board-note_expand_index_01 {},step_0002_add-board-note_expand_validate {},step_0003_add-board-rank_expand {},step_0003_add-board-rank_expand_index_01 {}"
		if strings.Join(ledger, ",") != want {
			t.Errorf("the ledger is %v, want %s", ledger, want)
		}

		// The release before runs on the schema of the release with the steps: its read-only ledger check, its schema
		// generation check and its schema manifest accept the database, and it writes and reads as before.
		if _, err := up.Exec("ALTER ROLE " + pgx.Identifier{tenancy.RuntimeRole}.Sanitize() + " LOGIN PASSWORD 'tables-check'"); err != nil {
			t.Fatal(err)
		}
		runtime := upCfg.Copy()
		runtime.User, runtime.Password = tenancy.RuntimeRole, "tables-check"
		reader := stdlib.OpenDB(*runtime)
		defer reader.Close()
		if err := boardsbase.CheckLedger(ctx, up); err != nil {
			t.Errorf("the ledger check of the release before refuses the steps: %v", err)
		}
		if err := boardsbase.VerifySchema(ctx, up); err != nil {
			t.Errorf("the schema check of the release before refuses the database: %v", err)
		}
		// The schema manifest of the release before accepts the constant default of the column rank that a later add
		// gives, on the table of a declared entity (ST-GEN-Q15 (a)).
		if err := manifest.Check(ctx, reader, owned(boardsbase.SchemaManifest)); err != nil {
			t.Errorf("the schema manifest of the release before refuses the database after an add with a default: %v", err)
		}
		if err := manifest.Check(ctx, reader, owned(boards.SchemaManifest)); err != nil {
			t.Errorf("the schema manifest of the release with the steps refuses the database: %v", err)
		}
		if code := write(t, up, "tenant-a", `INSERT INTO public."boards" ("id", "title") VALUES ($1, 'second')`, uuid.Must(uuid.NewV7()).String()); code != "" {
			t.Errorf("the release before writes no board after the steps: %s", code)
		}
		// The new table has the tenant wall of a table of the baseline: a card of one tenant is not seen by another.
		if code := write(t, up, "tenant-a", `INSERT INTO public."cards" ("id", "board_id", "code") VALUES ($1, $2, 'c1')`, uuid.Must(uuid.NewV7()).String(), first); code != "" {
			t.Fatalf("the release with the steps writes no card: %s", code)
		}
		if code := write(t, up, "tenant-b", `UPDATE public."cards" SET "code" = 'stolen'`); code != "" {
			t.Fatalf("an update of another tenant: %s", code)
		}
		var code string
		if err := up.QueryRow(`SELECT "code" FROM public."cards"`).Scan(&code); err != nil || code != "c1" {
			t.Errorf("another tenant changed the card to %q (error %v)", code, err)
		}
		if code := write(t, up, "tenant-b", `INSERT INTO public."cards" ("tenant_id", "id", "board_id") VALUES ('tenant-a', $1, $2)`, uuid.Must(uuid.NewV7()).String(), first); code != "42501" {
			t.Errorf("a card written into another tenant gives %q, want 42501", code)
		}
		// A card of one tenant cannot refer to a board of another: the foreign key holds the tenant column.
		if code := write(t, up, "tenant-b", `INSERT INTO public."cards" ("id", "board_id") VALUES ($1, $2)`, uuid.Must(uuid.NewV7()).String(), first); code != "23503" {
			t.Errorf("a card of tenant-b that refers to a board of tenant-a gives %q, want 23503", code)
		}
		// The table of the step has the tenant wall of a table of the baseline: the same row-level security, the
		// same policy text and the same grants.
		wall := func(table string) string {
			var state string
			if err := up.QueryRow(`SELECT c.relrowsecurity::text || '|' || c.relforcerowsecurity::text || '|' || coalesce(c.relacl::text, '') || '|' ||
  coalesce((SELECT pg_catalog.string_agg(p.policyname || ' ' || p.permissive || ' ' || p.roles::text || ' ' || p.cmd || ' ' || coalesce(p.qual, '') || ' ' || coalesce(p.with_check, ''), ';' ORDER BY p.policyname)
    FROM pg_catalog.pg_policies p WHERE p.schemaname = 'public' AND p.tablename = c.relname), '')
FROM pg_catalog.pg_class c WHERE c.oid = $1::pg_catalog.regclass`, "public."+table).Scan(&state); err != nil {
				t.Fatal(err)
			}
			return state
		}
		if cards, boards := wall("cards"), wall("boards"); cards != boards || !strings.HasPrefix(cards, "true|true|") || !strings.Contains(cards, "stego_tenant") {
			t.Errorf("the tenant wall of the step table cards is %q, want that of the baseline table boards, %q", cards, boards)
		}
		// The migration job of the release before runs again, as in a rollback of the job, and changes nothing.
		before := catalog(t, up)
		migrate(t, upCfg, up, func(ctx context.Context, conn *sql.Conn) error { return boardsbase.Migrate(ctx, conn) }, "boardsbase_access.json")
		if after := catalog(t, up); strings.Join(before, "\n") != strings.Join(after, "\n") {
			t.Error("the migration job of the release before changed the database after the steps")
		}

		// Adds with no default, here of the table cards, the optional field note and the optional reference parent_id,
		// whose foreign key the release before accepts in its exact form (ST-SUP-RP15-LC03), and the optional fields
		// note, level, score and mood, whose CHECK constraints it accepts in their exact forms (ST-SUP-RP15-LC01-T11):
		// the release before restarts on the database after the add migrations, and its ledger, schema and manifest
		// checks accept it.
		noteCfg, noted := database(t, cfg, admin)
		migrate(t, noteCfg, noted, func(ctx context.Context, conn *sql.Conn) error { return boardsbase.Migrate(ctx, conn) }, "boardsbase_access.json")
		migrate(t, noteCfg, noted, func(ctx context.Context, conn *sql.Conn) error { return boardsnote.Migrate(ctx, conn) }, "boardsnote_access.json")
		var checks string
		if err := noted.QueryRow(`SELECT pg_catalog.string_agg(conname, ',' ORDER BY conname) FROM pg_catalog.pg_constraint
  WHERE conrelid = 'public.boards'::pg_catalog.regclass AND contype = 'c'`).Scan(&checks); err != nil || checks != "boards_level_check,boards_mood_check,boards_note_check,boards_score_check" {
			t.Errorf("the add steps give the CHECK constraints %q (error %v), want those of level, mood, note and score", checks, err)
		}
		if _, err := noted.Exec("ALTER ROLE " + pgx.Identifier{tenancy.RuntimeRole}.Sanitize() + " LOGIN PASSWORD 'tables-check'"); err != nil {
			t.Fatal(err)
		}
		noteRuntime := noteCfg.Copy()
		noteRuntime.User, noteRuntime.Password = tenancy.RuntimeRole, "tables-check"
		noteReader := stdlib.OpenDB(*noteRuntime)
		defer noteReader.Close()
		if err := boardsbase.CheckLedger(ctx, noted); err != nil {
			t.Errorf("the ledger check of the release before refuses the adds with no default: %v", err)
		}
		if err := boardsbase.VerifySchema(ctx, noted); err != nil {
			t.Errorf("the schema check of the release before refuses the adds with no default: %v", err)
		}
		if err := manifest.Check(ctx, noteReader, owned(boardsbase.SchemaManifest)); err != nil {
			t.Errorf("the schema manifest of the release before refuses the adds with no default: %v", err)
		}
		if err := manifest.Check(ctx, noteReader, owned(boardsnote.SchemaManifest)); err != nil {
			t.Errorf("the schema manifest of the release with the adds refuses the database: %v", err)
		}

		// A new database that the migrations of the release with the steps make has the same schema (ST-GEN-INV35).
		freshCfg, fresh := database(t, cfg, admin)
		migrate(t, freshCfg, fresh, func(ctx context.Context, conn *sql.Conn) error { return boards.Migrate(ctx, conn) }, "boards_access.json")
		freshCatalog, upCatalog := catalog(t, fresh), catalog(t, up)
		if strings.Join(freshCatalog, "\n") != strings.Join(upCatalog, "\n") {
			t.Errorf("a new database and an upgraded one differ (ST-GEN-INV35):\nnew:\n%s\nupgraded:\n%s",
				strings.Join(freshCatalog, "\n"), strings.Join(upCatalog, "\n"))
		}
	})

	t.Run("ST-SUP-RC01-OC22-T04 A step waits for a lock only within its lock timeout and runs again, and a reader keeps getting its row", func(t *testing.T) {
		ctx := context.Background()
		lockCfg, locked := database(t, cfg, admin)
		migrate(t, lockCfg, locked, func(ctx context.Context, conn *sql.Conn) error { return boardsbase.Migrate(ctx, conn) }, "boardsbase_access.json")
		first := uuid.Must(uuid.NewV7()).String()
		if code := write(t, locked, "tenant-a", `INSERT INTO public."boards" ("id", "title") VALUES ($1, 'first')`, first); code != "" {
			t.Fatalf("the release before writes no board: %s", code)
		}
		// A session of the release before holds a lock on boards in an open transaction, as a long read does.
		holder, err := locked.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer holder.Close()
		if _, err := holder.ExecContext(ctx, `BEGIN; LOCK TABLE public."boards" IN ACCESS SHARE MODE`); err != nil {
			t.Fatal(err)
		}
		job := stdlib.OpenDB(*lockCfg)
		defer job.Close()
		conn, err := job.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := conn.ExecContext(ctx, "SET ROLE "+pgx.Identifier{tenancy.OwnerRole}.Sanitize()); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- boards.Migrate(ctx, conn) }()
		// The step waits for its ACCESS EXCLUSIVE lock behind the holder.
		deadline := time.Now().Add(10 * time.Second)
		for {
			var waiting bool
			if err := locked.QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_locks WHERE relation = 'public.boards'::pg_catalog.regclass AND mode = 'AccessExclusiveLock' AND NOT granted)`).Scan(&waiting); err != nil {
				t.Fatal(err)
			}
			if waiting {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the step never waits for its lock on boards")
			}
			time.Sleep(20 * time.Millisecond)
		}
		// A reader that comes after the waiting step gets its row within the lock timeout of the step and a margin,
		// and never waits for the holder: the step gives up its place in the queue when its lock timeout ends.
		started := time.Now()
		for i := 0; i < 3; i++ {
			var title string
			tx, err := locked.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(`SET LOCAL statement_timeout = '2500ms'`); err != nil {
				t.Fatal(err)
			}
			err = tx.QueryRow(`SELECT "title" FROM public."boards" WHERE "id" = $1`, first).Scan(&title)
			_ = tx.Rollback()
			if err != nil {
				t.Fatalf("a read of the release before while the step waits: %v", err)
			}
			if title != "first" {
				t.Fatalf("a read of the release before gives %q", title)
			}
			time.Sleep(500 * time.Millisecond)
		}
		// The step runs again after each lock timeout and does not fail while the holder holds its lock.
		if wait := time.Since(started); wait < 2*time.Second {
			time.Sleep(2*time.Second - wait)
		}
		select {
		case err := <-done:
			t.Fatalf("the migration job ended while the holder held its lock, with %v; want it to wait and run again", err)
		default:
		}
		if _, err := holder.ExecContext(ctx, `COMMIT`); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("the migration job after the holder ended: %v", err)
			}
		case <-time.After(60 * time.Second):
			t.Fatal("the migration job did not end after the holder ended")
		}
		var rows int
		if err := locked.QueryRow(`SELECT count(*) FROM stego_schema.migrations WHERE version LIKE 'step_%'`).Scan(&rows); err != nil || rows != 6 {
			t.Errorf("the ledger holds %d step rows (error %v), want 6", rows, err)
		}
	})

	t.Run("ST-SUP-RC01-OC22-T05 An add step on a table with rows scans no row under its strong lock, and the release before restarts after each phase", func(t *testing.T) {
		ctx := context.Background()
		stepCfg, stepped := database(t, cfg, admin)
		migrate(t, stepCfg, stepped, func(ctx context.Context, conn *sql.Conn) error { return boardsbase.Migrate(ctx, conn) }, "boardsbase_access.json")
		if _, err := stepped.Exec(`INSERT INTO public."boards" ("tenant_id", "id", "title") SELECT 'tenant-a', gen_random_uuid(), 'b' || g FROM generate_series(1, 2000) AS g`); err != nil {
			t.Fatal(err)
		}
		if _, err := stepped.Exec("ALTER ROLE " + pgx.Identifier{tenancy.RuntimeRole}.Sanitize() + " LOGIN PASSWORD 'tables-check'"); err != nil {
			t.Fatal(err)
		}
		runtime := stepCfg.Copy()
		runtime.User, runtime.Password = tenancy.RuntimeRole, "tables-check"
		reader := stdlib.OpenDB(*runtime)
		defer reader.Close()
		// The migrations of the add steps of boardsnote in the order of the ledger, as the files give them; each phase
		// is applied as the owner role, and the release before restarts after each one.
		names, err := os.ReadDir("../boardsnote/migrations")
		if err != nil {
			t.Fatal(err)
		}
		job := stdlib.OpenDB(*stepCfg)
		defer job.Close()
		conn, err := job.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := conn.ExecContext(ctx, "SET ROLE "+pgx.Identifier{tenancy.OwnerRole}.Sanitize()); err != nil {
			t.Fatal(err)
		}
		var phases []string
		for _, entry := range names {
			name := strings.TrimSuffix(entry.Name(), ".sql")
			if !strings.HasPrefix(name, "step_") {
				continue
			}
			body, err := os.ReadFile("../boardsnote/migrations/" + entry.Name())
			if err != nil {
				t.Fatal(err)
			}
			phases = append(phases, name)
			switch {
			case strings.Contains(name, "_index_"):
				if !strings.HasPrefix(string(body), "CREATE INDEX CONCURRENTLY IF NOT EXISTS ") && !strings.HasPrefix(string(body), "CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS ") {
					t.Errorf("%s does not build its index concurrently: %s", name, body)
				}
				if _, err := conn.ExecContext(ctx, string(body)); err != nil {
					t.Fatalf("%s: %v", name, err)
				}
			case strings.HasSuffix(name, "_validate"):
				if _, err := conn.ExecContext(ctx, string(body)); err != nil {
					t.Fatalf("%s: %v", name, err)
				}
			default:
				// The migration of the step itself: in one transaction, it scans no row of boards and builds no
				// index, while it holds the ACCESS EXCLUSIVE lock of an ALTER TABLE (decision on finding F2).
				tx, err := conn.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				// The view holds the counts of the session that are not yet reported, so the step's are the difference.
				const scanned = `SELECT coalesce((SELECT seq_scan + coalesce(idx_scan, 0) FROM pg_catalog.pg_stat_xact_user_tables WHERE relid = 'public.boards'::pg_catalog.regclass), 0)`
				var before int64
				if err := tx.QueryRowContext(ctx, scanned).Scan(&before); err != nil {
					_ = tx.Rollback()
					t.Fatal(err)
				}
				if _, err := tx.ExecContext(ctx, string(body)); err != nil {
					_ = tx.Rollback()
					t.Fatalf("%s: %v", name, err)
				}
				var scans int64
				var modes string
				if err := tx.QueryRowContext(ctx, scanned+`,
  coalesce((SELECT pg_catalog.string_agg(mode, ',' ORDER BY mode) FROM pg_catalog.pg_locks WHERE pid = pg_catalog.pg_backend_pid() AND relation = 'public.boards'::pg_catalog.regclass), '')`).Scan(&scans, &modes); err != nil {
					_ = tx.Rollback()
					t.Fatal(err)
				}
				if scans -= before; scans != 0 {
					t.Errorf("%s scans boards %d times while it holds the locks %s; want no scan", name, scans, modes)
				}
				for _, line := range strings.Split(string(body), "\n") {
					if strings.HasPrefix(line, "CREATE") && strings.Contains(line, " INDEX ") && strings.Contains(line, `ON public."boards"`) {
						t.Errorf("%s builds an index of boards in the transaction of the step: %s", name, line)
					}
				}
				// Each CHECK and foreign key that the step adds to boards is not yet validated: PostgreSQL validates a
				// constraint that is not NOT VALID by a scan in the transaction that adds it.
				var added, notValid int
				if err := tx.QueryRowContext(ctx, `SELECT count(*), count(*) FILTER (WHERE NOT convalidated) FROM pg_catalog.pg_constraint
  WHERE conrelid = 'public.boards'::pg_catalog.regclass AND contype IN ('c', 'f') AND conname = ANY ($1)`, constraintsOf(string(body))).Scan(&added, &notValid); err != nil {
					_ = tx.Rollback()
					t.Fatal(err)
				}
				if added != notValid || added != len(constraintsOf(string(body))) {
					t.Errorf("%s adds %d constraints to boards, of which %d are not yet validated; want each of %v NOT VALID", name, added, notValid, constraintsOf(string(body)))
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
			}
			if err := boardsbase.VerifySchema(ctx, stepped); err != nil {
				t.Errorf("after %s the schema check of the release before refuses the database: %v", name, err)
			}
			if err := manifest.Check(ctx, reader, owned(boardsbase.SchemaManifest)); err != nil {
				t.Errorf("after %s the schema manifest of the release before refuses the database: %v", name, err)
			}
		}
		if len(phases) < 6 || !slices.ContainsFunc(phases, func(n string) bool { return strings.HasSuffix(n, "_validate") }) || !slices.ContainsFunc(phases, func(n string) bool { return strings.Contains(n, "_index_") }) {
			t.Errorf("the add steps of boardsnote have the migrations %v; want each phase", phases)
		}
		// After every phase the constraints are validated, and the release with the steps accepts the database.
		var invalid int
		if err := stepped.QueryRow(`SELECT count(*) FROM pg_catalog.pg_constraint WHERE conrelid = 'public.boards'::pg_catalog.regclass AND NOT convalidated`).Scan(&invalid); err != nil || invalid != 0 {
			t.Errorf("%d constraints of boards are not validated after the steps (error %v)", invalid, err)
		}
		if err := manifest.Check(ctx, reader, owned(boardsnote.SchemaManifest)); err != nil {
			t.Errorf("the schema manifest of the release with the steps refuses the database: %v", err)
		}
	})

	t.Run("ST-SUP-RC01-OC38-T02 An index of an add step is built concurrently, and a failed build drops its invalid index at once", func(t *testing.T) {
		ctx := context.Background()
		slugCfg, slugged := database(t, cfg, admin)
		migrate(t, slugCfg, slugged, func(ctx context.Context, conn *sql.Conn) error { return boardsbase.Migrate(ctx, conn) }, "boardsbase_access.json")
		if _, err := slugged.Exec(`INSERT INTO public."boards" ("tenant_id", "id", "title") VALUES ('tenant-a', gen_random_uuid(), 'a'), ('tenant-a', gen_random_uuid(), 'b')`); err != nil {
			t.Fatal(err)
		}
		// A database event trigger gives both rows one slug right after the ALTER TABLE of the step, so the unique
		// build that follows fails on a duplicate after it made its index.
		if _, err := slugged.Exec(`CREATE FUNCTION public.dup_slug() RETURNS event_trigger LANGUAGE plpgsql SECURITY DEFINER AS $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_catalog.pg_attribute WHERE attrelid = 'public.boards'::pg_catalog.regclass AND attname = 'slug') THEN
    UPDATE public."boards" SET "slug" = 'same';
  END IF;
END $$;
CREATE EVENT TRIGGER dup_slug ON ddl_command_end WHEN TAG IN ('ALTER TABLE') EXECUTE FUNCTION public.dup_slug();`); err != nil {
			t.Fatal(err)
		}
		job := stdlib.OpenDB(*slugCfg)
		defer job.Close()
		conn, err := job.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := conn.ExecContext(ctx, "SET ROLE "+pgx.Identifier{tenancy.OwnerRole}.Sanitize()); err != nil {
			t.Fatal(err)
		}
		index := func() (bool, bool) {
			var present, valid bool
			if err := slugged.QueryRow(`SELECT count(*) > 0, coalesce(bool_and(i.indisvalid), false) FROM pg_catalog.pg_index i JOIN pg_catalog.pg_class c ON c.oid = i.indexrelid
  WHERE c.relname = 'boards_slug_key'`).Scan(&present, &valid); err != nil {
				t.Fatal(err)
			}
			return present, valid
		}
		ledger := func(version string) bool {
			var found bool
			if err := slugged.QueryRow(`SELECT EXISTS (SELECT 1 FROM stego_schema.migrations WHERE version = $1)`, version).Scan(&found); err != nil {
				t.Fatal(err)
			}
			return found
		}
		// The step builds the search index of slug, then its unique index.
		const step, build = "step_0001_add-board-slug_expand", "step_0001_add-board-slug_expand_index_02"
		if err := boardsslug.Migrate(ctx, conn); err == nil || !strings.Contains(err.Error(), build) {
			t.Fatalf("the migration job with a duplicate gives %v, want the failure of %s", err, build)
		}
		if present, _ := index(); present {
			t.Error("the failed build left its invalid index; want it dropped at once (decision on finding F9)")
		}
		if !ledger(step) || ledger(build) {
			t.Errorf("after the failed build the ledger holds the step %v and the build %v; want the step only", ledger(step), ledger(build))
		}
		// The job holds no lock after it ends: neither the ledger lock of its session nor a transaction. Only the
		// locks of this test's database count: the jobs of other tests run on the same server at the same time.
		var held int
		if err := slugged.QueryRow(`SELECT count(*) FROM pg_catalog.pg_locks WHERE locktype = 'advisory' AND granted
  AND database = (SELECT oid FROM pg_catalog.pg_database WHERE datname = pg_catalog.current_database())`).Scan(&held); err != nil || held != 0 {
			t.Errorf("after the failed build %d advisory locks are held (error %v), want none", held, err)
		}
		// An invalid index of the same name that a build which stopped part way left is dropped before the next build.
		if _, err := slugged.Exec(`DROP EVENT TRIGGER dup_slug`); err != nil {
			t.Fatal(err)
		}
		if _, err := slugged.Exec(`CREATE UNIQUE INDEX CONCURRENTLY boards_slug_key ON public."boards" ("tenant_id", "slug")`); err == nil {
			t.Fatal("a unique build over the duplicate succeeded")
		}
		if present, valid := index(); !present || valid {
			t.Fatalf("the stand-in for a stopped build left the index present %v, valid %v; want an invalid index", present, valid)
		}
		if _, err := slugged.Exec(`UPDATE public."boards" SET "slug" = NULL`); err != nil {
			t.Fatal(err)
		}
		if err := boardsslug.Migrate(ctx, conn); err != nil {
			t.Fatalf("the migration job after the duplicate is gone: %v", err)
		}
		if present, valid := index(); !present || !valid || !ledger(build) {
			t.Errorf("after the second job the index is present %v and valid %v, and the ledger holds the build %v; want all", present, valid, ledger(build))
		}
		if _, err := slugged.Exec("DROP FUNCTION public.dup_slug(); ALTER ROLE " + pgx.Identifier{tenancy.RuntimeRole}.Sanitize() + " LOGIN PASSWORD 'tables-check'"); err != nil {
			t.Fatal(err)
		}
		runtime := slugCfg.Copy()
		runtime.User, runtime.Password = tenancy.RuntimeRole, "tables-check"
		reader := stdlib.OpenDB(*runtime)
		defer reader.Close()
		if err := manifest.Check(ctx, reader, owned(boardsslug.SchemaManifest)); err != nil {
			t.Errorf("the schema manifest of the release with the step refuses the database: %v", err)
		}
		// The release before does not name the unique index of slug, and accepts it in the exact form of a later add
		// step: the tenant column first, on the column that it does not list.
		if err := manifest.Check(ctx, reader, owned(boardsbase.SchemaManifest)); err != nil {
			t.Errorf("the schema manifest of the release before refuses the database after the step: %v", err)
		}
	})

	// slugJob makes a database that the Board release before migrated, with two boards, and gives the session of the
	// migration job of the release with the add of Board.slug as the owner role.
	slugJob := func(t *testing.T) (*sql.DB, *sql.Conn) {
		t.Helper()
		ctx := context.Background()
		slugCfg, slugged := database(t, cfg, admin)
		migrate(t, slugCfg, slugged, func(ctx context.Context, conn *sql.Conn) error { return boardsbase.Migrate(ctx, conn) }, "boardsbase_access.json")
		if _, err := slugged.Exec(`INSERT INTO public."boards" ("tenant_id", "id", "title") VALUES ('tenant-a', gen_random_uuid(), 'a'), ('tenant-a', gen_random_uuid(), 'b')`); err != nil {
			t.Fatal(err)
		}
		job := stdlib.OpenDB(*slugCfg)
		t.Cleanup(func() { _ = job.Close() })
		conn, err := job.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		if _, err := conn.ExecContext(ctx, "SET ROLE "+pgx.Identifier{tenancy.OwnerRole}.Sanitize()); err != nil {
			t.Fatal(err)
		}
		return slugged, conn
	}
	ledgerHolds := func(t *testing.T, db *sql.DB, version string) bool {
		t.Helper()
		var found bool
		if err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM stego_schema.migrations WHERE version = $1)`, version).Scan(&found); err != nil {
			t.Fatal(err)
		}
		return found
	}
	indexState := func(t *testing.T, db *sql.DB, name string) (bool, bool) {
		t.Helper()
		var present, valid bool
		if err := db.QueryRow(`SELECT count(*) > 0, coalesce(bool_and(i.indisvalid), false) FROM pg_catalog.pg_index i JOIN pg_catalog.pg_class c ON c.oid = i.indexrelid
  WHERE c.relname = $1`, name).Scan(&present, &valid); err != nil {
			t.Fatal(err)
		}
		return present, valid
	}
	const slugSearch, slugUnique = "step_0001_add-board-slug_expand_index_01", "step_0001_add-board-slug_expand_index_02"

	t.Run("ST-SUP-RC01-OC38-T03 An index build waits for an older transaction with no lock timeout, and is built once", func(t *testing.T) {
		ctx := context.Background()
		busy, conn := slugJob(t)
		// A database event trigger counts each CREATE INDEX and DROP INDEX, so a build that ends at the lock timeout and
		// runs again shows.
		if _, err := busy.Exec(`CREATE SEQUENCE public.index_builds; CREATE SEQUENCE public.index_drops;
CREATE FUNCTION public.count_index() RETURNS event_trigger LANGUAGE plpgsql SECURITY DEFINER AS $$
BEGIN
  IF tg_tag = 'CREATE INDEX' THEN PERFORM nextval('public.index_builds'); ELSE PERFORM nextval('public.index_drops'); END IF;
END $$;
CREATE EVENT TRIGGER count_index ON ddl_command_start WHEN TAG IN ('CREATE INDEX', 'DROP INDEX') EXECUTE FUNCTION public.count_index();`); err != nil {
			t.Fatal(err)
		}
		// A transaction of another session holds a snapshot for 3 s, longer than the lock timeout of a step. It holds
		// no lock on boards, so the ALTER TABLE of the step does not wait for it; CREATE INDEX CONCURRENTLY does.
		holder, err := busy.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer holder.Close()
		if _, err := holder.ExecContext(ctx, `BEGIN ISOLATION LEVEL REPEATABLE READ; SELECT count(*) FROM pg_catalog.pg_class`); err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		done := make(chan error, 1)
		go func() { done <- boardsslug.Migrate(ctx, conn) }()
		time.Sleep(3*time.Second - time.Since(started))
		select {
		case err := <-done:
			t.Fatalf("the migration job ended while an older transaction was open, with %v; want the build to wait for it", err)
		default:
		}
		if _, err := holder.ExecContext(ctx, `COMMIT`); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("the migration job after the older transaction ended: %v", err)
			}
		case <-time.After(60 * time.Second):
			t.Fatal("the migration job did not end after the older transaction ended")
		}
		var builds, drops int64
		if err := busy.QueryRow(`SELECT (SELECT CASE WHEN is_called THEN last_value ELSE 0 END FROM public.index_builds), (SELECT CASE WHEN is_called THEN last_value ELSE 0 END FROM public.index_drops)`).Scan(&builds, &drops); err != nil {
			t.Fatal(err)
		}
		if builds != 2 || drops != 0 {
			t.Errorf("the job ran %d index builds and %d drops, want 2 builds, one for each index of the step, and no drop", builds, drops)
		}
		for _, name := range []string{"boards_slug_search", "boards_slug_key"} {
			if present, valid := indexState(t, busy, name); !present || !valid {
				t.Errorf("the index %s is present %v and valid %v, want both", name, present, valid)
			}
		}
		if !ledgerHolds(t, busy, slugSearch) || !ledgerHolds(t, busy, slugUnique) {
			t.Error("the ledger does not hold the rows of both index builds")
		}
	})

	t.Run("ST-SUP-RC01-OC38-T04 A failed drop of the invalid index after a failed build runs again, and when it still fails the job fails and names the index", func(t *testing.T) {
		ctx := context.Background()
		for _, c := range []struct {
			name     string
			failures int
		}{{"a drop that fails twice", 2}, {"a drop that always fails", 1000}} {
			t.Run(c.name, func(t *testing.T) {
				db, conn := slugJob(t)
				// The event trigger of ST-SUP-RC01-OC38-T02 makes the unique build fail on a duplicate, and a second one
				// refuses the first drops of an index. It counts each drop with a session advisory lock of key
				// (4242, n), which a rollback does not release. It does not count with nextval: after a checkpoint,
				// nextval logs the sequence and gives the transaction of the drop an ID, and then DROP INDEX
				// CONCURRENTLY fails as it is not the first action of its transaction, so a drop that the trigger
				// let through failed and ran again.
				if _, err := db.Exec(fmt.Sprintf(`CREATE FUNCTION public.dup_slug() RETURNS event_trigger LANGUAGE plpgsql SECURITY DEFINER AS $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_catalog.pg_attribute WHERE attrelid = 'public.boards'::pg_catalog.regclass AND attname = 'slug') THEN
    UPDATE public."boards" SET "slug" = 'same';
  END IF;
END $$;
CREATE EVENT TRIGGER dup_slug ON ddl_command_end WHEN TAG IN ('ALTER TABLE') EXECUTE FUNCTION public.dup_slug();
CREATE FUNCTION public.fail_drop() RETURNS event_trigger LANGUAGE plpgsql SECURITY DEFINER AS $$
DECLARE
  drop_number bigint;
BEGIN
  SELECT count(*) + 1 INTO drop_number FROM pg_catalog.pg_locks
    WHERE locktype = 'advisory' AND classid = 4242 AND objsubid = 2 AND pid = pg_catalog.pg_backend_pid();
  PERFORM pg_catalog.pg_advisory_lock(4242, drop_number::integer);
  IF drop_number <= %d THEN
    RAISE EXCEPTION 'the test refuses the drop';
  END IF;
END $$;
CREATE EVENT TRIGGER fail_drop ON ddl_command_start WHEN TAG IN ('DROP INDEX') EXECUTE FUNCTION public.fail_drop();`, c.failures)); err != nil {
					t.Fatal(err)
				}
				err := boardsslug.Migrate(ctx, conn)
				if err == nil || !strings.Contains(err.Error(), slugUnique) {
					t.Fatalf("the migration job with a duplicate gives %v, want the failure of %s", err, slugUnique)
				}
				var drops int64
				if err := db.QueryRow(`SELECT count(*) FROM pg_catalog.pg_locks WHERE locktype = 'advisory' AND classid = 4242 AND objsubid = 2`).Scan(&drops); err != nil {
					t.Fatal(err)
				}
				present, valid := indexState(t, db, "boards_slug_key")
				if c.failures == 2 {
					if present || drops != 3 || strings.Contains(err.Error(), "drops of it failed") {
						t.Errorf("after two failed drops the index is present %v after %d drops, and the job gives %v; want it dropped by the third drop and only the build failure", present, drops, err)
					}
				} else if !present || valid || !strings.Contains(err.Error(), `the failed build left the invalid index public."boards_slug_key", and 5 drops of it failed`) {
					t.Errorf("when each drop fails the index is present %v and valid %v, and the job gives %v; want the invalid index named in the error after 5 drops", present, valid, err)
				}
				if ledgerHolds(t, db, slugUnique) {
					t.Error("the ledger holds the failed build")
				}
			})
		}
	})

	t.Run("ST-SUP-RC01-OC38-T05 An index build fails when a relation of its name is there and is not the index of the step", func(t *testing.T) {
		ctx := context.Background()
		for _, c := range []struct{ name, setup, failed string }{
			{"a table of the name of the search index", `CREATE TABLE public."boards_slug_search" (x integer)`, slugSearch},
			{"an index of the name of the unique index with another definition", `CREATE INDEX "boards_slug_key" ON public."boards" ("tenant_id")`, slugUnique},
		} {
			t.Run(c.name, func(t *testing.T) {
				db, conn := slugJob(t)
				if _, err := db.Exec(c.setup); err != nil {
					t.Fatal(err)
				}
				if err := boardsslug.Migrate(ctx, conn); err == nil || !strings.Contains(err.Error(), c.failed) {
					t.Fatalf("the migration job gives %v, want the failure of %s", err, c.failed)
				}
				if ledgerHolds(t, db, c.failed) {
					t.Errorf("the ledger holds %s, whose index the step did not build", c.failed)
				}
			})
		}
		// A rerun after a build that succeeded and wrote no ledger row finds the index of the step and records it.
		t.Run("the index of the step from a build that wrote no ledger row", func(t *testing.T) {
			db, conn := slugJob(t)
			if err := boardsslug.Migrate(ctx, conn); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`DELETE FROM stego_schema.migrations WHERE version >= $1`, slugUnique); err != nil {
				t.Fatal(err)
			}
			if err := boardsslug.Migrate(ctx, conn); err != nil || !ledgerHolds(t, db, slugUnique) {
				t.Errorf("the rerun gives %v and the ledger holds the build %v; want no error and the row", err, ledgerHolds(t, db, slugUnique))
			}
		})
	})

	t.Run("ST-SUP-RC01-OC42-T02 The marker check accepts the digest of the stored shape and the one a release before wrote, and no other", func(t *testing.T) {
		ctx := context.Background()
		markCfg, mark := database(t, cfg, admin)
		migrate(t, markCfg, mark, func(ctx context.Context, conn *sql.Conn) error { return boards.Migrate(ctx, conn) }, "boards_access.json")
		var definition string
		if err := mark.QueryRow(`SELECT definition FROM stego_schema.generation`).Scan(&definition); err != nil || definition != boards.SchemaDefinition {
			t.Fatalf("a new database holds the marker digest %q (error %v), want the digest of the stored shape %s", definition, err, boards.SchemaDefinition)
		}
		before := os.Getenv("STEGO_TEST_MARKER_BEFORE")
		if len(before) != 64 || before == boards.SchemaDefinition {
			t.Fatalf("the digest of a release before is %q", before)
		}
		// A database that a release before ST-SUP-RC01-OC42 made holds the digest of the entities and collections.
		if _, err := mark.Exec(`UPDATE stego_schema.generation SET definition = $1`, before); err != nil {
			t.Fatal(err)
		}
		if err := boards.VerifySchema(ctx, mark); err != nil {
			t.Errorf("the marker check refuses the digest that a release before wrote: %v", err)
		}
		migrate(t, markCfg, mark, func(ctx context.Context, conn *sql.Conn) error { return boards.Migrate(ctx, conn) }, "boards_access.json")
		if _, err := mark.Exec(`UPDATE stego_schema.generation SET definition = $1`, strings.Repeat("0", 64)); err != nil {
			t.Fatal(err)
		}
		if err := boards.VerifySchema(ctx, mark); !errors.Is(err, boards.ErrSchemaGeneration) {
			t.Errorf("the marker check gives %v for the digest of another declaration, want ErrSchemaGeneration", err)
		}
	})

	t.Run("ST-SUP-RC01-OC42-T03 The migration job rewrites the marker digest of a release before to the digest of the stored shape once", func(t *testing.T) {
		ctx := context.Background()
		markCfg, mark := database(t, cfg, admin)
		migrate(t, markCfg, mark, func(ctx context.Context, conn *sql.Conn) error { return boards.Migrate(ctx, conn) }, "boards_access.json")
		before := os.Getenv("STEGO_TEST_MARKER_BEFORE")
		if len(before) != 64 || before == boards.SchemaDefinition {
			t.Fatalf("the digest of a release before is %q", before)
		}
		// A database that a release before ST-SUP-RC01-OC42 made holds the digest of the entities and collections.
		if _, err := mark.Exec(`UPDATE stego_schema.generation SET definition = $1`, before); err != nil {
			t.Fatal(err)
		}
		var versions int
		if err := mark.QueryRow(`SELECT count(*) FROM stego_schema.migrations`).Scan(&versions); err != nil {
			t.Fatal(err)
		}
		migrate(t, markCfg, mark, func(ctx context.Context, conn *sql.Conn) error { return boards.Migrate(ctx, conn) }, "boards_access.json")
		var definition string
		var after int
		if err := mark.QueryRow(`SELECT definition, (SELECT count(*) FROM stego_schema.migrations) FROM stego_schema.generation`).Scan(&definition, &after); err != nil {
			t.Fatal(err)
		}
		if definition != boards.SchemaDefinition || after != versions {
			t.Errorf("after the migration job the marker holds %q and the ledger %d rows; want the digest of the stored shape %s and the %d rows before", definition, after, boards.SchemaDefinition, versions)
		}
		if err := boards.VerifySchema(ctx, mark); err != nil {
			t.Errorf("the marker check refuses the rewritten digest: %v", err)
		}
		// A digest that is neither stays as it is: the job rewrites only the digest of a release before.
		other := strings.Repeat("0", 64)
		if _, err := mark.Exec(`UPDATE stego_schema.generation SET definition = $1`, other); err != nil {
			t.Fatal(err)
		}
		job := stdlib.OpenDB(*markCfg)
		defer job.Close()
		conn, err := job.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := conn.ExecContext(ctx, "SET ROLE "+pgx.Identifier{tenancy.OwnerRole}.Sanitize()); err != nil {
			t.Fatal(err)
		}
		if err := boards.Migrate(ctx, conn); !errors.Is(err, boards.ErrSchemaGeneration) {
			t.Errorf("the migration job gives %v on the digest of another declaration, want ErrSchemaGeneration", err)
		}
		if err := mark.QueryRow(`SELECT definition FROM stego_schema.generation`).Scan(&definition); err != nil || definition != other {
			t.Errorf("the migration job changed the digest of another declaration to %q (error %v)", definition, err)
		}
		// A job that fails its ledger check, here on an edited migration, leaves the digest of a release before as it
		// is, so the pods of that release can still restart.
		if _, err := mark.Exec(`UPDATE stego_schema.generation SET definition = $1`, before); err != nil {
			t.Fatal(err)
		}
		if _, err := mark.Exec(`UPDATE stego_schema.migrations SET digest = $1 WHERE version = '001_initial'`, strings.Repeat("f", 64)); err != nil {
			t.Fatal(err)
		}
		if err := boards.Migrate(ctx, conn); !errors.Is(err, boards.ErrMigrationLedger) {
			t.Errorf("the migration job gives %v on an edited migration, want ErrMigrationLedger", err)
		}
		if err := mark.QueryRow(`SELECT definition FROM stego_schema.generation`).Scan(&definition); err != nil || definition != before {
			t.Errorf("a job that fails its ledger check left the marker %q (error %v), want the digest of a release before", definition, err)
		}
	})
}

// constraintsOf gives the names of the constraints that the statements of body add to the table boards.
func constraintsOf(body string) []string {
	var names []string
	for _, line := range strings.Split(body, "\n") {
		if rest, ok := strings.CutPrefix(line, `ALTER TABLE public."boards" ADD CONSTRAINT "`); ok {
			names = append(names, rest[:strings.Index(rest, `"`)])
		}
	}
	return names
}

// owned gives the generated manifest m with the owner role of the tenancy component as its owner, as the service
// runtime gives it (service.checkSchema), so the held schema of the sync functions has an owner to check.
func owned(m manifest.Manifest) manifest.Manifest {
	m.Owner = tenancy.OwnerRole
	return m
}
