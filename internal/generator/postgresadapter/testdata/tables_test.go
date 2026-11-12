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

	"example.com/tables-check/previous"
	"example.com/tables-check/storage"
	"example.com/tables-check/tenancy"
	"example.com/tables-check/widgets"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/jsell-rh/stego/registry/runtime/manifest"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
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
func migrate(t *testing.T, cfg *pgx.ConnConfig, admin *sql.DB, run func(*gorm.DB) error, accessFile string) {
	t.Helper()
	job := stdlib.OpenDB(*cfg)
	job.SetMaxOpenConns(1)
	defer job.Close()
	if _, err := job.Exec("SET ROLE " + pgx.Identifier{tenancy.OwnerRole}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: job}), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := run(db); err != nil {
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
		migrate(t, freshCfg, fresh, storage.Migrate, "access.json")

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
		migrate(t, upgradedCfg, upgraded, previous.Migrate, "access.json")
		insertOrder(t, upgraded, "tenant-a", "sku-1")
		migrate(t, upgradedCfg, upgraded, storage.Migrate, "access.json")
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
		migrate(t, cfg, db, widgets.Migrate, "widgets_access.json")
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
		migrate(t, cfg, db, widgets.Migrate, "widgets_access.json")
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
		if _, err := db.Exec(`ALTER TABLE public."orders" DROP CONSTRAINT "orders_widget_id_fkey"`); err != nil {
			t.Fatal(err)
		}
		if err := manifest.Check(ctx, reader, widgets.SchemaManifest); !errors.Is(err, manifest.ErrNotSatisfied) ||
			!strings.Contains(err.Error(), "table public.orders: constraint orders_widget_id_fkey is missing") {
			t.Fatalf("a dropped foreign key gave %v, want the fault that names it", err)
		}
	})
}
