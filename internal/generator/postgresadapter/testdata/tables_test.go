package check_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"example.com/tables-check/boards"
	"example.com/tables-check/boardsbase"
	"example.com/tables-check/bounds"
	"example.com/tables-check/previous"
	"example.com/tables-check/storage"
	"example.com/tables-check/tenancy"
	"example.com/tables-check/widgets"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
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
		if _, err := admin.Exec("DROP DATABASE " + pgx.Identifier{name}.Sanitize() + " WITH (FORCE)"); err != nil {
			t.Error(err)
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
		if err := manifest.Check(ctx, reader, widgets.SchemaManifest); err != nil {
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
		if err := manifest.Check(ctx, boundsReader, bounds.SchemaManifest); err != nil {
			t.Fatalf("the database of the bounds does not hold the schema manifest: %v", err)
		}
		if _, err := db.Exec(`ALTER TABLE public."orders" DROP CONSTRAINT "orders_widget_id_fkey"`); err != nil {
			t.Fatal(err)
		}
		if err := manifest.Check(ctx, reader, widgets.SchemaManifest); !errors.Is(err, manifest.ErrNotSatisfied) ||
			!strings.Contains(err.Error(), "table public.orders: constraint orders_widget_id_fkey is missing") {
			t.Fatalf("a dropped foreign key gave %v, want the fault that names it", err)
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
		want := "001_initial none,100_tenancy_row_security none,step_0001_add-card_expand {},step_0002_add-board-note_expand {},step_0003_add-board-rank_expand {}"
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
		if err := manifest.Check(ctx, reader, boardsbase.SchemaManifest); err != nil {
			t.Errorf("the schema manifest of the release before refuses the database: %v", err)
		}
		if err := manifest.Check(ctx, reader, boards.SchemaManifest); err != nil {
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

		// A new database that the migrations of the release with the steps make has the same schema (ST-GEN-INV35).
		freshCfg, fresh := database(t, cfg, admin)
		migrate(t, freshCfg, fresh, func(ctx context.Context, conn *sql.Conn) error { return boards.Migrate(ctx, conn) }, "boards_access.json")
		freshCatalog, upCatalog := catalog(t, fresh), catalog(t, up)
		if strings.Join(freshCatalog, "\n") != strings.Join(upCatalog, "\n") {
			t.Errorf("a new database and an upgraded one differ (ST-GEN-INV35):\nnew:\n%s\nupgraded:\n%s",
				strings.Join(freshCatalog, "\n"), strings.Join(upCatalog, "\n"))
		}
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
	})
}
