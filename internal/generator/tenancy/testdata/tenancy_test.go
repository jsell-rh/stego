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
	"example.com/tenancy-check/tenancyb"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/jsell-rh/stego/registry/runtime/authz"
	"github.com/jsell-rh/stego/registry/runtime/outbox"
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
		for _, role := range []string{tenancy.RuntimeRole, tenancy.WorkerRole, tenancy.RelayRole, tenancy.PurgeRole, tenancy.OutboxRelayRole, tenancy.OwnerRole} {
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

	t.Run("ST-SUP-RC27-OC04-T05 The role migration makes the worker, relay, purge and outbox relay roles with the owner's SET memberships", func(t *testing.T) {
		for _, role := range []string{tenancy.WorkerRole, tenancy.RelayRole} {
			var inherit, bypass, super bool
			if err := db.QueryRowContext(ctx, "SELECT rolinherit, rolbypassrls, rolsuper FROM pg_roles WHERE rolname = $1", role).Scan(&inherit, &bypass, &super); err != nil {
				t.Fatalf("role %s: %v", role, err)
			}
			if inherit || bypass || super {
				t.Errorf("role %s: INHERIT %v, BYPASSRLS %v, superuser %v", role, inherit, bypass, super)
			}
		}
		// The idempotency purge role (ST-SUP-RP07-LC11) and the outbox relay role (ST-SUP-RP01-LC19) are the
		// service's own, inherit nothing, cannot log in or bypass row-level security, have no CREATE on public, and
		// the owner may only SET them; no runtime, worker or relay role is a member.
		base := strings.TrimSuffix(tenancy.RuntimeRole, "_runtime")
		if tenancy.PurgeRole != base+"_idempotency_purge" || tenancy.OutboxRelayRole != base+"_outbox_relay" {
			t.Errorf("the purge role is %s and the outbox relay role %s, want the service's own", tenancy.PurgeRole, tenancy.OutboxRelayRole)
		}
		for _, own := range []string{tenancy.PurgeRole, tenancy.OutboxRelayRole} {
			var login, ownInherit, ownBypass, ownSuper bool
			if err := db.QueryRowContext(ctx, "SELECT rolcanlogin, rolinherit, rolbypassrls, rolsuper FROM pg_roles WHERE rolname = $1", own).
				Scan(&login, &ownInherit, &ownBypass, &ownSuper); err != nil || login || ownInherit || ownBypass || ownSuper {
				t.Errorf("%s: LOGIN %v, INHERIT %v, BYPASSRLS %v, superuser %v, %v", own, login, ownInherit, ownBypass, ownSuper, err)
			}
			var set, inherit bool
			if err := db.QueryRowContext(ctx, `SELECT m.set_option, m.inherit_option FROM pg_auth_members m
 JOIN pg_roles g ON g.oid = m.roleid JOIN pg_roles r ON r.oid = m.member
 WHERE g.rolname = $1 AND r.rolname = $2`, own, tenancy.OwnerRole).Scan(&set, &inherit); err != nil || !set || inherit {
				t.Errorf("the owner role's membership in %s: SET %v, INHERIT %v, want SET only: %v", own, set, inherit, err)
			}
			var create bool
			if err := db.QueryRowContext(ctx, "SELECT has_schema_privilege($1, 'public', 'CREATE')", own).Scan(&create); err != nil || create {
				t.Errorf("%s has CREATE on public: %v, %v", own, create, err)
			}
			for _, role := range []string{tenancy.RuntimeRole, tenancy.WorkerRole, tenancy.RelayRole} {
				var member bool
				if err := db.QueryRowContext(ctx, "SELECT pg_has_role($1, $2, 'MEMBER')", role, own).Scan(&member); err != nil || member {
					t.Errorf("the role %s is a member of %s: %v, %v", role, own, member, err)
				}
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

// database creates a fresh database on the server, runs roles in it as the administrator, and gives its name.
func database(t *testing.T, server *sql.DB, cfg *pgx.ConnConfig, roles string) string {
	t.Helper()
	ctx := context.Background()
	name := "tenancy_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := server.ExecContext(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := server.ExecContext(context.Background(), "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Error(err)
		}
	})
	c := cfg.Copy()
	c.Database = name
	admin := stdlib.OpenDB(*c)
	defer admin.Close()
	if _, err := admin.ExecContext(ctx, roles); err != nil {
		t.Fatalf("role migration: %v", err)
	}
	return name
}

func TestTwoServices(t *testing.T) {
	t.Run("ST-SUP-RC27-OC04-T06 Two services in one cluster cannot reach each other's databases", func(t *testing.T) {
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
		ctx := context.Background()
		rolesA := []string{tenancy.RuntimeRole, tenancy.WorkerRole, tenancy.RelayRole, tenancy.PurgeRole, tenancy.OutboxRelayRole, tenancy.OwnerRole}
		rolesB := []string{tenancyb.RuntimeRole, tenancyb.WorkerRole, tenancyb.RelayRole, tenancyb.PurgeRole, tenancyb.OutboxRelayRole, tenancyb.OwnerRole}
		// The roles belong to the server: drop them after the databases.
		t.Cleanup(func() {
			for _, role := range append(append([]string{}, rolesA...), rolesB...) {
				_, _ = server.ExecContext(context.Background(), "DROP ROLE IF EXISTS "+pgx.Identifier{role}.Sanitize())
			}
		})
		a := database(t, server, cfg, tenancy.Roles)
		b := database(t, server, cfg, tenancyb.Roles)
		if tenancy.PurgeRole == tenancyb.PurgeRole {
			t.Fatalf("the two services share the purge role %s", tenancy.PurgeRole)
		}
		const password = "two-services"
		for _, role := range []string{tenancy.OwnerRole, tenancy.RuntimeRole, tenancy.WorkerRole, tenancy.RelayRole} {
			if _, err := server.ExecContext(ctx, "ALTER ROLE "+pgx.Identifier{role}.Sanitize()+" LOGIN PASSWORD '"+password+"'"); err != nil {
				t.Fatal(err)
			}
		}
		connect := func(role, database string) error {
			c := cfg.Copy()
			c.User, c.Password, c.Database = role, password, database
			conn, err := pgx.ConnectConfig(ctx, c)
			if err == nil {
				_ = conn.Close(ctx)
			}
			return err
		}
		// Each role of service A connects to its own database and to no other: PUBLIC has no CONNECT.
		for _, role := range []string{tenancy.OwnerRole, tenancy.RuntimeRole, tenancy.WorkerRole, tenancy.RelayRole} {
			if err := connect(role, a); err != nil {
				t.Fatalf("%s cannot connect to its own database: %v", role, err)
			}
			if err := connect(role, b); !denied(err) {
				t.Fatalf("%s of service A connected to the database of service B: %v", role, err)
			}
		}
		var public bool
		if err := server.QueryRowContext(ctx, "SELECT has_database_privilege('public', $1, 'CONNECT')", b).Scan(&public); err != nil || public {
			t.Fatalf("PUBLIC keeps CONNECT on the database of service B: %v, %v", public, err)
		}
		// The owner of A is no member of the purge role of B, so it cannot read or delete B's key rows or replace
		// its purge function through it; and it only SETs its own purge role, which gives it none of its rights.
		var memberB, usageA, setA bool
		if err := server.QueryRowContext(ctx, `SELECT pg_has_role($1, $2, 'MEMBER'), pg_has_role($1, $3, 'USAGE'), pg_has_role($1, $3, 'SET')`,
			tenancy.OwnerRole, tenancyb.PurgeRole, tenancy.PurgeRole).Scan(&memberB, &usageA, &setA); err != nil || memberB || usageA || !setA {
			t.Fatalf("the owner of A: member of B's purge role %v, privileges of its own purge role %v, SET of it %v: %v", memberB, usageA, setA, err)
		}
	})
}

func TestOpenSessions(t *testing.T) {
	t.Run("ST-SUP-RC27-OC04-T07 The role migration refuses while a session of another role is open in the database", func(t *testing.T) {
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
		ctx := context.Background()
		suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
		name, foreign := "tenancy_open_"+suffix, "foreign_"+suffix
		const password = "open-sessions"
		if _, err := server.ExecContext(ctx, "CREATE ROLE "+foreign+" LOGIN PASSWORD '"+password+"'"); err != nil {
			t.Fatal(err)
		}
		if _, err := server.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = server.ExecContext(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)")
			for _, role := range []string{foreign, tenancy.RuntimeRole, tenancy.WorkerRole, tenancy.RelayRole, tenancy.PurgeRole, tenancy.OutboxRelayRole, tenancy.OwnerRole} {
				_, _ = server.ExecContext(context.Background(), "DROP ROLE IF EXISTS "+pgx.Identifier{role}.Sanitize())
			}
		})
		// A role of no service opens a session in the new database before the role migration, and keeps it.
		fc := cfg.Copy()
		fc.User, fc.Password, fc.Database = foreign, password, name
		held, err := pgx.ConnectConfig(ctx, fc)
		if err != nil {
			t.Fatal(err)
		}
		ac := cfg.Copy()
		ac.Database = name
		admin := stdlib.OpenDB(*ac)
		admin.SetMaxOpenConns(1)
		t.Cleanup(func() { admin.Close() })
		_, err = admin.ExecContext(ctx, tenancy.Roles)
		if err == nil || !strings.Contains(err.Error(), "1 sessions of roles that are not roles of this service") || strings.Contains(err.Error(), foreign) {
			t.Fatalf("the role migration with a foreign session open gave %v, want a refusal that counts it and names no role", err)
		}
		// The session is refused nothing and is not terminated: the migration only refuses.
		if err := held.Ping(ctx); err != nil {
			t.Fatalf("the foreign session was ended: %v", err)
		}
		// The revoke of CONNECT holds, so the role can open no new session.
		if c, err := pgx.ConnectConfig(ctx, fc); err == nil {
			_ = c.Close(ctx)
			t.Fatal("the foreign role opened a new session after the revoke")
		} else if !denied(err) {
			t.Fatalf("a new foreign session gave %v, want a refusal of CONNECT", err)
		}
		if err := held.Close(ctx); err != nil {
			t.Fatal(err)
		}
		// The server ends the backend a moment after the client closes; the migration then passes.
		deadline := time.Now().Add(10 * time.Second)
		for {
			_, err = admin.ExecContext(ctx, tenancy.Roles)
			if err == nil || time.Now().After(deadline) {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if err != nil {
			t.Fatalf("the role migration after the foreign session closed: %v", err)
		}
	})
}

// service holds the roles and the role migration of one generated service of the relay test.
type service struct {
	owner, runtime, worker, relay, purge, outboxRelay, roles string
}

// migrations gives the outbox and authorization migrations of the runtime library for the roles of s, in the order
// of the migration job.
func (s service) migrations(t *testing.T) []string {
	t.Helper()
	queue, err := outbox.Migration(s.outboxRelay, s.runtime, s.worker)
	if err != nil {
		t.Fatal(err)
	}
	tables, err := authz.Migration("public", s.runtime, s.relay, s.outboxRelay, s.worker)
	if err != nil {
		t.Fatal(err)
	}
	return []string{queue, tables}
}

// apply runs each text in a transaction of its own on db, as role when it is not empty, as the migration job does.
func apply(t *testing.T, db *sql.DB, role string, texts []string) {
	t.Helper()
	for _, text := range texts {
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if role != "" {
			if _, err := tx.Exec("SET LOCAL ROLE " + pgx.Identifier{role}.Sanitize()); err != nil {
				_ = tx.Rollback()
				t.Fatal(err)
			}
		}
		if _, err := tx.Exec(text); err != nil {
			_ = tx.Rollback()
			t.Fatalf("migration: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
}

// enqueue writes one pending message of the tenant t1 as the administrator, past row-level security.
func enqueue(t *testing.T, admin *sql.DB) {
	t.Helper()
	id := uuid.New()
	if _, err := admin.Exec(`INSERT INTO stego_outbox.messages (id, tenant_id, destination, resource_key, kind, payload,
 outgoing_key, created_at, available_at) VALUES ($1, 't1', 'billing', 'k1', 'record.created', '\x7b7d', $2, now(), now())`, id, id.String()); err != nil {
		t.Fatal(err)
	}
}

func TestRelayRoles(t *testing.T) {
	t.Run("ST-SUP-RC27-OC04-T08 The outbox relay role is each service's own, and the owner of one service reaches no outbox of another", func(t *testing.T) {
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
		ctx := context.Background()
		a := service{tenancy.OwnerRole, tenancy.RuntimeRole, tenancy.WorkerRole, tenancy.RelayRole, tenancy.PurgeRole, tenancy.OutboxRelayRole, tenancy.Roles}
		b := service{tenancyb.OwnerRole, tenancyb.RuntimeRole, tenancyb.WorkerRole, tenancyb.RelayRole, tenancyb.PurgeRole, tenancyb.OutboxRelayRole, tenancyb.Roles}
		// The roles belong to the server: drop them after the databases.
		t.Cleanup(func() {
			for _, s := range []service{a, b} {
				for _, role := range []string{s.runtime, s.worker, s.relay, s.purge, s.outboxRelay, s.owner} {
					_, _ = server.ExecContext(context.Background(), "DROP ROLE IF EXISTS "+pgx.Identifier{role}.Sanitize())
				}
			}
		})
		if a.outboxRelay == b.outboxRelay {
			t.Fatalf("the two services share the outbox relay role %s", a.outboxRelay)
		}
		const password = "relay-roles"
		open := func(role, database string) *sql.DB {
			c := cfg.Copy()
			c.Database = database
			if role != "" {
				c.User, c.Password = role, password
			}
			db := stdlib.OpenDB(*c)
			db.SetMaxOpenConns(1)
			t.Cleanup(func() { db.Close() })
			return db
		}
		nameA := database(t, server, cfg, a.roles)
		login := func(role string) {
			t.Helper()
			if _, err := server.ExecContext(ctx, "ALTER ROLE "+pgx.Identifier{role}.Sanitize()+" LOGIN PASSWORD '"+password+"'"); err != nil {
				t.Fatal(err)
			}
		}
		login(a.owner)
		// The database of B is new, so PUBLIC still has CONNECT: the owner of A opens a session in it before B's role
		// migration, and keeps it.
		nameB := "tenancy_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		if _, err := server.ExecContext(ctx, "CREATE DATABASE "+pgx.Identifier{nameB}.Sanitize()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = server.ExecContext(context.Background(), "DROP DATABASE "+pgx.Identifier{nameB}.Sanitize()+" WITH (FORCE)")
		})
		hc := cfg.Copy()
		hc.User, hc.Password, hc.Database = a.owner, password, nameB
		held, err := pgx.ConnectConfig(ctx, hc)
		if err != nil {
			t.Fatalf("the owner of A opens a session in the new database of B: %v", err)
		}
		t.Cleanup(func() { _ = held.Close(context.Background()) })
		adminB := open("", nameB)
		if _, err := adminB.ExecContext(ctx, b.roles); err == nil || !strings.Contains(err.Error(), "1 sessions of roles that are not roles of this service") {
			t.Fatalf("B's role migration with the session of A's owner open gave %v, want the refusal", err)
		}
		// An administrator who goes past the refusal runs the rest of B's role migration, then B's migrations as B's
		// owner, and B's queue gets a message, while the session of A's owner stays open.
		rest := b.roles[strings.LastIndex(b.roles, "END\n$$;\n")+len("END\n$$;\n"):]
		if !strings.HasPrefix(rest, "ALTER ROLE ") {
			t.Fatalf("the rest of B's role migration is %.40q", rest)
		}
		if _, err := adminB.ExecContext(ctx, rest); err != nil {
			t.Fatal(err)
		}
		apply(t, adminB, b.owner, b.migrations(t))
		enqueue(t, adminB)
		// Every path from the session of A's owner to B's messages and to B's relay and state hook functions is
		// refused: as itself, as its own outbox relay role, and by taking, granting or changing B's roles and functions.
		claim := "stego_outbox.claim(timestamptz, integer, uuid, bigint)"
		for name, attempt := range map[string]struct{ role, statement string }{
			"read B's messages":                      {"", "SELECT count(*) FROM stego_outbox.messages"},
			"run B's claim":                          {"", "SELECT count(*) FROM stego_outbox.claim(NULL, 1, gen_random_uuid(), 60000)"},
			"run B's purge":                          {"", "SELECT stego_outbox.purge_done(NULL, 1, 0)"},
			"run B's state hook":                     {"", "SELECT public.authz_revocation_state(gen_random_uuid(), gen_random_uuid(), 'parked')"},
			"read B's messages as A's relay role":    {a.outboxRelay, "SELECT count(*) FROM stego_outbox.messages"},
			"run B's claim as A's relay role":        {a.outboxRelay, "SELECT count(*) FROM stego_outbox.claim(NULL, 1, gen_random_uuid(), 60000)"},
			"run B's state hook as A's relay role":   {a.outboxRelay, "SELECT public.authz_revocation_state(gen_random_uuid(), gen_random_uuid(), 'parked')"},
			"take B's outbox relay role":             {"", "SET ROLE " + pgx.Identifier{b.outboxRelay}.Sanitize()},
			"take B's owner role":                    {"", "SET ROLE " + pgx.Identifier{b.owner}.Sanitize()},
			"grant itself B's outbox relay role":     {"", "GRANT " + pgx.Identifier{b.outboxRelay}.Sanitize() + " TO " + pgx.Identifier{a.owner}.Sanitize()},
			"give B's claim to A's relay role":       {"", "ALTER FUNCTION " + claim + " OWNER TO " + pgx.Identifier{a.outboxRelay}.Sanitize()},
			"give B's claim to A's relay role as it": {a.outboxRelay, "ALTER FUNCTION " + claim + " OWNER TO " + pgx.Identifier{a.outboxRelay}.Sanitize()},
			"give A's relay role B's messages":       {"", "GRANT SELECT ON stego_outbox.messages TO " + pgx.Identifier{a.outboxRelay}.Sanitize()},
		} {
			tx, err := held.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if attempt.role != "" {
				if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+pgx.Identifier{attempt.role}.Sanitize()); err != nil {
					t.Fatalf("%s: the owner of A cannot take its own relay role: %v", name, err)
				}
			}
			_, err = tx.Exec(ctx, attempt.statement)
			_ = tx.Rollback(ctx)
			if !denied(err) {
				t.Errorf("the session of A's owner in B's database could %s: %v", name, err)
			}
		}
		if err := held.Ping(ctx); err != nil {
			t.Fatalf("the session of A's owner was ended: %v", err)
		}
		var memberOwner, memberRelay, intact bool
		if err := adminB.QueryRowContext(ctx, `SELECT pg_has_role($1, $3, 'MEMBER'), pg_has_role($2, $3, 'MEMBER'),
 (SELECT count(*) = 1 AND bool_and(state = 'pending' AND lease_token IS NULL) FROM stego_outbox.messages)`,
			a.owner, a.outboxRelay, b.outboxRelay).Scan(&memberOwner, &memberRelay, &intact); err != nil || memberOwner || memberRelay || !intact {
			t.Fatalf("A's owner a member of B's relay role %v, A's relay role a member of it %v, B's message intact %v: %v", memberOwner, memberRelay, intact, err)
		}
		if err := held.Close(ctx); err != nil {
			t.Fatal(err)
		}
		// With the session closed, B's role migration passes; then B's owner, no superuser, runs B's migrations
		// twice with SET membership only in B's relay role, and A's owner does the same in A's database.
		deadline := time.Now().Add(10 * time.Second)
		for {
			_, err = adminB.ExecContext(ctx, b.roles)
			if err == nil || time.Now().After(deadline) {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if err != nil {
			t.Fatalf("B's role migration after the session closed: %v", err)
		}
		login(b.owner)
		for _, side := range []struct {
			s    service
			name string
		}{{a, nameA}, {b, nameB}} {
			owner := open(side.s.owner, side.name)
			apply(t, owner, "", side.s.migrations(t))
			apply(t, owner, "", side.s.migrations(t))
			admin := open("", side.name)
			var owners string
			if err := admin.QueryRowContext(ctx, `SELECT string_agg(DISTINCT pg_get_userbyid(p.proowner), ',') FROM pg_proc p
 JOIN pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname = 'stego_outbox' OR p.proname IN ('authz_revocation_state', 'authz_revocation_tuple_check')`).
				Scan(&owners); err != nil || owners != side.s.outboxRelay {
				t.Errorf("the relay and state hook functions of %s belong to %s, want %s: %v", side.name, owners, side.s.outboxRelay, err)
			}
			var create bool
			if err := admin.QueryRowContext(ctx, "SELECT has_schema_privilege($1, 'public', 'CREATE') OR has_schema_privilege($1, 'stego_outbox', 'CREATE')",
				side.s.outboxRelay).Scan(&create); err != nil || create {
				t.Errorf("the relay role %s keeps CREATE after the migrations: %v, %v", side.s.outboxRelay, create, err)
			}
			if side.name != nameA {
				continue
			}
			// Inside its own database the owner does not inherit the rights of its relay role: it sees no message of
			// any tenant unless it takes the role with SET ROLE, as the migration does.
			enqueue(t, admin)
			var seen, asRelay int
			if err := owner.QueryRowContext(ctx, "SELECT count(*) FROM stego_outbox.messages").Scan(&seen); err != nil || seen != 0 {
				t.Errorf("A's owner as itself sees %d messages, want 0: %v", seen, err)
			}
			tx, err := owner.Begin()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec("SET LOCAL ROLE " + pgx.Identifier{a.outboxRelay}.Sanitize()); err != nil {
				t.Fatal(err)
			}
			err = tx.QueryRow("SELECT count(*) FROM stego_outbox.messages").Scan(&asRelay)
			_ = tx.Rollback()
			if err != nil || asRelay != 1 {
				t.Errorf("A's owner as its relay role sees %d messages, want 1: %v", asRelay, err)
			}
		}
	})
}

func TestForeignRoles(t *testing.T) {
	t.Run("ST-SUP-RC27-OC04-T10 The role migration refuses a role of its names that is not this service's alone", func(t *testing.T) {
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
		ctx := context.Background()
		exec := func(statement string) {
			t.Helper()
			if _, err := server.ExecContext(ctx, statement); err != nil {
				t.Fatalf("%s: %v", statement, err)
			}
		}
		// The database of another service, on which a role of one of these names holds CONNECT.
		other := "tenancy_other_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
		exec("CREATE DATABASE " + other)
		t.Cleanup(func() {
			_, _ = server.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+other+" WITH (FORCE)")
		})
		foreign := "foreign_group_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
		exec("CREATE ROLE " + foreign + " NOLOGIN")
		t.Cleanup(func() { _, _ = server.ExecContext(context.Background(), "DROP ROLE IF EXISTS "+foreign) })
		q := func(role string) string { return pgx.Identifier{role}.Sanitize() }
		for _, c := range []struct {
			role, setup, fault string
			kept               string // a query that is true while the role keeps what it had
		}{
			{tenancy.OwnerRole, "CREATE ROLE " + q(tenancy.OwnerRole) + " LOGIN; GRANT CONNECT ON DATABASE " + other + " TO " + q(tenancy.OwnerRole),
				"holds a privilege on another database, which is not a database of this service", "SELECT has_database_privilege('" + tenancy.OwnerRole + "', '" + other + "', 'CONNECT') AND rolcanlogin FROM pg_roles WHERE rolname = '" + tenancy.OwnerRole + "'"},
			{tenancy.OutboxRelayRole, "CREATE ROLE " + q(tenancy.OutboxRelayRole) + " LOGIN",
				"can log in", "SELECT rolcanlogin FROM pg_roles WHERE rolname = '" + tenancy.OutboxRelayRole + "'"},
			{tenancy.RuntimeRole, "CREATE ROLE " + q(tenancy.RuntimeRole) + " NOLOGIN IN ROLE " + foreign,
				"is a member of a role that this service does not grant", "SELECT pg_has_role('" + tenancy.RuntimeRole + "', '" + foreign + "', 'MEMBER')"},
		} {
			exec(c.setup)
			name := "tenancy_foreign_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
			exec("CREATE DATABASE " + name)
			ac := cfg.Copy()
			ac.Database = name
			admin := stdlib.OpenDB(*ac)
			_, err := admin.ExecContext(ctx, tenancy.Roles)
			admin.Close()
			if err == nil || !strings.Contains(err.Error(), c.role) || !strings.Contains(err.Error(), c.fault) {
				t.Errorf("the role migration with a role %s that %s gave %v, want a refusal that names both", c.role, c.fault, err)
			}
			var kept, public bool
			if err := server.QueryRowContext(ctx, c.kept).Scan(&kept); err != nil || !kept {
				t.Errorf("the refused migration changed the role %s: %v, %v", c.role, kept, err)
			}
			// The refusal comes before the revoke of CONNECT from PUBLIC, so the text changed nothing.
			if err := server.QueryRowContext(ctx, "SELECT has_database_privilege('public', $1, 'CONNECT')", name).Scan(&public); err != nil || !public {
				t.Errorf("the refused migration revoked CONNECT on %s: %v, %v", name, public, err)
			}
			exec("DROP DATABASE " + name + " WITH (FORCE)")
			exec("REVOKE ALL ON DATABASE " + other + " FROM " + q(c.role))
			exec("DROP ROLE " + q(c.role))
		}
		// A login with CREATEROLE that made the owner and runtime roles first holds ADMIN on them as a member, and a
		// role of another service granted the runtime role is a member of it: each is refused with the member named
		// (finding G1 of the security review of PR #213).
		prov := "prov_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
		exec("CREATE ROLE " + prov + " LOGIN CREATEROLE PASSWORD 'prov'")
		t.Cleanup(func() { _, _ = server.ExecContext(context.Background(), "DROP ROLE IF EXISTS "+prov) })
		pc := cfg.Copy()
		pc.User, pc.Password = prov, "prov"
		provDB := stdlib.OpenDB(*pc)
		for _, role := range []string{tenancy.OwnerRole, tenancy.RuntimeRole} {
			if _, err := provDB.ExecContext(ctx, "CREATE ROLE "+q(role)+" NOLOGIN"); err != nil {
				t.Fatal(err)
			}
		}
		provDB.Close()
		member := "x_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12] + "_owner"
		exec("CREATE ROLE " + member + " NOLOGIN")
		t.Cleanup(func() { _, _ = server.ExecContext(context.Background(), "DROP ROLE IF EXISTS "+member) })
		run := func(setting string) error {
			t.Helper()
			name := "tenancy_member_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
			exec("CREATE DATABASE " + name)
			defer exec("DROP DATABASE " + name + " WITH (FORCE)")
			ac := cfg.Copy()
			ac.Database = name
			admin := stdlib.OpenDB(*ac)
			admin.SetMaxOpenConns(1)
			defer admin.Close()
			if setting != "" {
				if _, err := admin.ExecContext(ctx, "SET stego.migration_login = '"+setting+"'"); err != nil {
					t.Fatal(err)
				}
			}
			_, err := admin.ExecContext(ctx, tenancy.Roles)
			return err
		}
		if err := run(""); err == nil || !strings.Contains(err.Error(), "has the members "+prov+" with ADMIN") {
			t.Errorf("the role migration over roles that %s made gave %v, want a refusal that names %s with ADMIN", prov, err, prov)
		}
		exec("DROP ROLE " + q(tenancy.OwnerRole))
		exec("DROP ROLE " + q(tenancy.RuntimeRole))
		exec("CREATE ROLE " + q(tenancy.RuntimeRole) + " NOLOGIN")
		exec("GRANT " + q(tenancy.RuntimeRole) + " TO " + member)
		if err := run(""); err == nil || !strings.Contains(err.Error(), tenancy.RuntimeRole) || !strings.Contains(err.Error(), "has the members "+member) {
			t.Errorf("the role migration over a runtime role that %s is a member of gave %v, want a refusal that names both", member, err)
		}
		exec("DROP ROLE " + q(tenancy.RuntimeRole))
		// The owner role may have one member: the migration login that the operator names in stego.migration_login.
		login := "migrator_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
		exec("CREATE ROLE " + login + " LOGIN")
		t.Cleanup(func() { _, _ = server.ExecContext(context.Background(), "DROP ROLE IF EXISTS "+login) })
		exec("CREATE ROLE " + q(tenancy.OwnerRole) + " NOLOGIN")
		exec("GRANT " + q(tenancy.OwnerRole) + " TO " + login + " WITH SET TRUE, INHERIT FALSE")
		if err := run(""); err == nil || !strings.Contains(err.Error(), "has the members "+login) {
			t.Errorf("the role migration with a migration login that is not named gave %v, want a refusal that names it", err)
		}
		if err := run(login); err != nil {
			t.Errorf("the role migration with the migration login named in stego.migration_login: %v", err)
		}
		exec("GRANT " + q(tenancy.OwnerRole) + " TO " + login + " WITH ADMIN TRUE")
		if err := run(login); err == nil || !strings.Contains(err.Error(), login+" with ADMIN") {
			t.Errorf("the role migration with a named migration login that holds ADMIN gave %v, want a refusal", err)
		}
		for _, role := range []string{tenancy.RuntimeRole, tenancy.WorkerRole, tenancy.RelayRole, tenancy.PurgeRole, tenancy.OutboxRelayRole, tenancy.OwnerRole} {
			exec("DROP ROLE IF EXISTS " + q(role))
		}
		// The role migration of this service runs on a second database of its own: the roles hold privileges on the
		// first, where the text gave the owner CREATE, so they are this service's.
		var names []string
		for range 2 {
			name := "tenancy_own_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
			exec("CREATE DATABASE " + name)
			names = append(names, name)
			ac := cfg.Copy()
			ac.Database = name
			admin := stdlib.OpenDB(*ac)
			_, err := admin.ExecContext(ctx, tenancy.Roles)
			admin.Close()
			if err != nil {
				t.Errorf("the role migration on database %d of this service: %v", len(names), err)
			}
		}
		for _, name := range names {
			exec("DROP DATABASE " + name + " WITH (FORCE)")
		}
		for _, role := range []string{tenancy.RuntimeRole, tenancy.WorkerRole, tenancy.RelayRole, tenancy.PurgeRole, tenancy.OutboxRelayRole, tenancy.OwnerRole} {
			exec("DROP ROLE IF EXISTS " + q(role))
		}
	})
}
