package check_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"example.com/tenancy-check/storage"
	"example.com/tenancy-check/tenancy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// owned are the tables of the owned entities Widget and Order, and others the tables of the shared entity
// Catalog and the system entity Mark.
var (
	owned  = []string{"widgets", "orders"}
	others = []string{"catalogs", "marks"}
)

// migrated creates a fresh database, runs the role migration as the administrator and the migrations of the
// service as the owner role, and returns the administrator's connection to that database.
func migrated(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("STEGO_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Fatal("STEGO_TEST_POSTGRES_DSN names no PostgreSQL server")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	server := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { server.Close() })
	// The roles belong to the server, not to the database: drop them after the database, so each run creates them.
	t.Cleanup(func() {
		for _, role := range []string{tenancy.RuntimeRole, tenancy.WorkerRole, tenancy.RelayRole, tenancy.OwnerRole} {
			if _, err := server.ExecContext(context.Background(), "DROP ROLE IF EXISTS "+pgx.Identifier{role}.Sanitize()); err != nil {
				t.Error(err)
			}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	name := "tenancy_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := server.ExecContext(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := server.ExecContext(context.Background(), "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Error(err)
		}
	})
	cfg.Database = name
	admin := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { admin.Close() })
	if _, err := admin.ExecContext(ctx, tenancy.Roles); err != nil {
		t.Fatalf("role migration: %v", err)
	}
	// One connection, so SET ROLE holds for every statement of the migration job.
	job := stdlib.OpenDB(*cfg)
	job.SetMaxOpenConns(1)
	defer job.Close()
	if _, err := job.ExecContext(ctx, "SET ROLE "+pgx.Identifier{tenancy.OwnerRole}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: job}), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.Migrate(db); err != nil {
		t.Fatalf("migrate as the owner role: %v", err)
	}
	grant(t, admin)
	return admin
}

// grant gives the runtime role the access that the adapter declares, as the database access contract does.
func grant(t *testing.T, admin *sql.DB) {
	t.Helper()
	data, err := os.ReadFile("access.json")
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
		if object.Kind != "table" || len(object.Privileges) == 0 {
			continue
		}
		statement := "GRANT " + strings.Join(object.Privileges, ", ") + " ON TABLE " + pgx.Identifier{object.Schema, object.Name}.Sanitize() + " TO " + pgx.Identifier{tenancy.RuntimeRole}.Sanitize()
		if _, err := admin.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

// as runs fn in one transaction as role, with the tenant setting of tenant when it is not empty, and rolls back.
func as(t *testing.T, db *sql.DB, role, tenant string, fn func(tx *sql.Tx) error) error {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("SET LOCAL ROLE " + pgx.Identifier{role}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	if tenant != "" {
		if _, err := tx.Exec("SELECT pg_catalog.set_config('app.tenant', $1, true)", tenant); err != nil {
			t.Fatal(err)
		}
	}
	return fn(tx)
}

// denied reports whether err is PostgreSQL's insufficient_privilege.
func denied(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42501"
}

// insertCatalog writes one shared Catalog row.
func insertCatalog(tx *sql.Tx, code string) error {
	_, err := tx.Exec("INSERT INTO public.catalogs (id, code, created_time, updated_time) VALUES ($1, $2, now(), now())", uuid.NewString(), code)
	return err
}

func TestTenancy(t *testing.T) {
	db := migrated(t)
	ctx := context.Background()

	t.Run("ST-SUP-RC27-OC03-T01 Every tenant-owned table has row-level security enabled and forced", func(t *testing.T) {
		for _, table := range owned {
			var enabled, forced bool
			if err := db.QueryRowContext(ctx, "SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE oid = $1::regclass", "public."+table).Scan(&enabled, &forced); err != nil {
				t.Fatal(err)
			}
			if !enabled || !forced {
				t.Errorf("table %s: row-level security enabled %v, forced %v", table, enabled, forced)
			}
		}
	})

	t.Run("ST-SUP-RC27-OC03-T02 Each tenant-owned table has one policy on tenant_id equal to the current setting", func(t *testing.T) {
		for _, table := range owned {
			rows, err := db.QueryContext(ctx, "SELECT coalesce(qual, ''), coalesce(with_check, '') FROM pg_policies WHERE schemaname = 'public' AND tablename = $1", table)
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			for rows.Next() {
				count++
				var using, check string
				if err := rows.Scan(&using, &check); err != nil {
					t.Fatal(err)
				}
				for _, expression := range []string{using, check} {
					if !strings.Contains(expression, "tenant_id = current_setting('app.tenant'") {
						t.Errorf("table %s: policy expression %q does not compare tenant_id with the tenant setting", table, expression)
					}
				}
			}
			rows.Close()
			if count != 1 {
				t.Errorf("table %s has %d policies, want 1", table, count)
			}
		}
	})

	t.Run("ST-SUP-RC27-OC04-T01 The role migration creates a runtime role and an owner role", func(t *testing.T) {
		for _, role := range []string{tenancy.RuntimeRole, tenancy.OwnerRole} {
			var found bool
			if err := db.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)", role).Scan(&found); err != nil || !found {
				t.Errorf("role %s: found %v, %v", role, found, err)
			}
		}
		for _, table := range append(append([]string{}, owned...), others...) {
			var owner string
			if err := db.QueryRowContext(ctx, "SELECT tableowner FROM pg_tables WHERE schemaname = 'public' AND tablename = $1", table).Scan(&owner); err != nil {
				t.Fatal(table, err)
			}
			if owner != tenancy.OwnerRole {
				t.Errorf("table %s is owned by %s, want %s", table, owner, tenancy.OwnerRole)
			}
		}
	})

	t.Run("ST-SUP-RC27-OC04-T05 The role migration makes the worker and relay roles, the relay membership and the CREATE grant", func(t *testing.T) {
		for _, role := range []string{tenancy.WorkerRole, tenancy.RelayRole} {
			var inherit, bypass, super bool
			if err := db.QueryRowContext(ctx, "SELECT rolinherit, rolbypassrls, rolsuper FROM pg_roles WHERE rolname = $1", role).Scan(&inherit, &bypass, &super); err != nil {
				t.Fatalf("role %s: %v", role, err)
			}
			if inherit || bypass || super {
				t.Errorf("role %s: INHERIT %v, BYPASSRLS %v, superuser %v", role, inherit, bypass, super)
			}
		}
		var set, inherit bool
		if err := db.QueryRowContext(ctx, `SELECT m.set_option, m.inherit_option FROM pg_auth_members m
 JOIN pg_roles g ON g.oid = m.roleid JOIN pg_roles r ON r.oid = m.member
 WHERE g.rolname = 'stego_outbox_relay' AND r.rolname = $1`, tenancy.OwnerRole).Scan(&set, &inherit); err != nil || !set || !inherit {
			t.Errorf("the owner role's membership in stego_outbox_relay: SET %v, INHERIT %v, %v", set, inherit, err)
		}
		var create bool
		if err := db.QueryRowContext(ctx, "SELECT has_schema_privilege('stego_outbox_relay', 'public', 'CREATE')").Scan(&create); err != nil || !create {
			t.Errorf("stego_outbox_relay has CREATE on public: %v, %v", create, err)
		}
		for _, role := range []string{tenancy.RuntimeRole, tenancy.WorkerRole, tenancy.RelayRole} {
			var member bool
			if err := db.QueryRowContext(ctx, "SELECT pg_has_role($1, 'stego_outbox_relay', 'MEMBER')", role).Scan(&member); err != nil || member {
				t.Errorf("the role %s is a member of stego_outbox_relay: %v, %v", role, member, err)
			}
		}
	})

	t.Run("ST-SUP-RC27-OC04-T03 The runtime role has no BYPASSRLS and owns no table", func(t *testing.T) {
		var bypass, super bool
		if err := db.QueryRowContext(ctx, "SELECT rolbypassrls, rolsuper FROM pg_roles WHERE rolname = $1", tenancy.RuntimeRole).Scan(&bypass, &super); err != nil {
			t.Fatal(err)
		}
		if bypass || super {
			t.Errorf("runtime role: BYPASSRLS %v, superuser %v", bypass, super)
		}
		var tables int
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM pg_class c JOIN pg_roles r ON r.oid = c.relowner WHERE r.rolname = $1", tenancy.RuntimeRole).Scan(&tables); err != nil {
			t.Fatal(err)
		}
		if tables != 0 {
			t.Errorf("the runtime role owns %d relations", tables)
		}
	})

	t.Run("ST-SUP-RC27-OC07-T01 Every tenant reads a shared entity", func(t *testing.T) {
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec("SET LOCAL ROLE " + pgx.Identifier{tenancy.OwnerRole}.Sanitize()); err != nil {
			t.Fatal(err)
		}
		for _, code := range []string{"read-1", "read-2"} {
			if err := insertCatalog(tx, code); err != nil {
				t.Fatal(err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		for _, tenant := range []string{"tenant-a", "tenant-b"} {
			var count int
			err := as(t, db, tenancy.RuntimeRole, tenant, func(tx *sql.Tx) error {
				return tx.QueryRow("SELECT count(*) FROM public.catalogs WHERE code IN ('read-1', 'read-2')").Scan(&count)
			})
			if err != nil || count != 2 {
				t.Errorf("tenant %s reads %d shared rows, error %v; want 2", tenant, count, err)
			}
		}
	})

	t.Run("ST-SUP-RC27-OC07-T02 Neither a tenant nor a system binding can write a shared entity", func(t *testing.T) {
		writes := map[string]func(tx *sql.Tx) error{
			"insert": func(tx *sql.Tx) error { return insertCatalog(tx, "write") },
			"update": func(tx *sql.Tx) error {
				_, err := tx.Exec("UPDATE public.catalogs SET code = 'changed'")
				return err
			},
		}
		for name, write := range writes {
			if err := as(t, db, tenancy.RuntimeRole, "tenant-a", write); !denied(err) {
				t.Errorf("the runtime role's %s of a shared row: %v, want a permission error", name, err)
			}
			if err := as(t, db, tenancy.OwnerRole, "", write); err != nil {
				t.Errorf("the owner role's %s of a shared row: %v", name, err)
			}
		}
	})

	t.Run("ST-SUP-RC01-OC40-T01 The tenant column leads the primary key and every index of an owned table", func(t *testing.T) {
		for _, table := range owned {
			rows, err := db.QueryContext(ctx, `SELECT ic.relname, i.indisprimary, a.attname
FROM pg_index i
JOIN pg_class ic ON ic.oid = i.indexrelid
JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = i.indkey[0]
WHERE i.indrelid = $1::regclass`, "public."+table)
			if err != nil {
				t.Fatal(err)
			}
			count, primary := 0, false
			for rows.Next() {
				var index, first string
				var isPrimary bool
				if err := rows.Scan(&index, &isPrimary, &first); err != nil {
					t.Fatal(err)
				}
				count++
				primary = primary || isPrimary
				if first != "tenant_id" {
					t.Errorf("table %s: index %s starts with %s, not tenant_id", table, index, first)
				}
			}
			rows.Close()
			// The primary key, the declared secondary index and the deleted_at index.
			if count < 3 || !primary {
				t.Errorf("table %s has %d indexes, primary key %v; want at least the primary key and two more", table, count, primary)
			}
		}
	})

	t.Run("ST-SUP-RC01-OC40-T02 A shared and a system entity carry no tenant column", func(t *testing.T) {
		for _, table := range others {
			var columns int
			if err := db.QueryRowContext(ctx, "SELECT count(*) FROM information_schema.columns WHERE table_schema = 'public' AND table_name = $1", table).Scan(&columns); err != nil {
				t.Fatal(err)
			}
			if columns == 0 {
				t.Fatalf("table %s does not exist", table)
			}
			var tenant int
			if err := db.QueryRowContext(ctx, "SELECT count(*) FROM information_schema.columns WHERE table_schema = 'public' AND table_name = $1 AND column_name = 'tenant_id'", table).Scan(&tenant); err != nil {
				t.Fatal(err)
			}
			if tenant != 0 {
				t.Errorf("table %s has a tenant column", table)
			}
		}
	})
}
