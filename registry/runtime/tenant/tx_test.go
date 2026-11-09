package tenant

import (
	"context"
	"database/sql"
	"errors"
	"go/ast"
	"go/token"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestTenantTransaction(t *testing.T) {
	t.Run("ST-SUP-RP04-LC03-T01 The first statement of every transaction sets the tenant transaction-locally", func(t *testing.T) {
		env := newPG(t)
		ctx := context.Background()
		for _, id := range []string{"tenant-a", "tenant-b"} {
			env.statements.reset()
			tx, err := Begin(ctx, env.runtimeDB, mustTenant(t, id), nil)
			if err != nil {
				t.Fatal(err)
			}
			var seen string
			if err := tx.QueryRowContext(ctx, "SELECT current_setting('app.tenant')").Scan(&seen); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			statements, args := env.statements.log()
			if len(statements) != 4 || !strings.HasPrefix(strings.ToLower(statements[0]), "begin") ||
				statements[1] != setTenantStatement ||
				statements[2] != "SELECT current_setting('app.tenant')" || statements[3] != "commit" {
				t.Fatalf("the statement log of the transaction of %s is %q, want begin, %s, the caller's SELECT, commit",
					id, statements, setTenantStatement)
			}
			if len(args[1]) == 0 || args[1][len(args[1])-1] != id {
				t.Fatalf("the tenant setting of the transaction of %s has the arguments %v", id, args[1])
			}
			if seen != id {
				t.Fatalf("the caller's SELECT in the transaction of %s read the tenant %q", id, seen)
			}
		}
	})

	t.Run("ST-SUP-RP04-LC03-T02 The helper runs no session-scoped tenant setting", func(t *testing.T) {
		env := newPG(t)
		ctx := context.Background()
		env.runtimeDB.SetMaxOpenConns(1)
		env.statements.reset()
		tx, err := Begin(ctx, env.runtimeDB, mustTenant(t, "tenant-a"), nil)
		if err != nil {
			t.Fatal(err)
		}
		var pidIn int
		if err := tx.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&pidIn); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		var pidAfter int
		var after sql.NullString
		if err := env.runtimeDB.QueryRowContext(ctx,
			"SELECT pg_backend_pid(), current_setting('app.tenant', true)").Scan(&pidAfter, &after); err != nil {
			t.Fatal(err)
		}
		if pidAfter != pidIn {
			t.Fatalf("the read after the commit ran on the connection %d, not on %d", pidAfter, pidIn)
		}
		if after.String != "" {
			t.Fatalf("after the commit the connection holds app.tenant = %q, want an empty value", after.String)
		}
		statements, _ := env.statements.log()
		for _, statement := range statements {
			if name, ok := sessionSetting(statement); ok {
				t.Errorf("the statement log holds the session setting %s: %q", name, statement)
			}
		}

		// A function that sets app.tenant with is_local false inside Each fails, and Each rolls its transaction back,
		// so the connection keeps no tenant.
		err = Each(ctx, env.runtimeDB, []Tenant{mustTenant(t, "tenant-a")}, func(ctx context.Context, tx Querier, _ Tenant) error {
			_, err := tx.ExecContext(ctx, "SELECT set_config('app.tenant', 'tenant-b', false)")
			return err
		})
		if !errors.Is(err, ErrTenantChanged) {
			t.Fatalf("Each committed a transaction whose function set the tenant tenant-b with is_local false: %v", err)
		}
		if err := env.runtimeDB.QueryRowContext(ctx, "SELECT current_setting('app.tenant', true)").Scan(&after); err != nil {
			t.Fatal(err)
		}
		if after.String != "" {
			t.Fatalf("after the refused commit the connection holds app.tenant = %q", after.String)
		}

		// The package source holds no session-scoped SET and no set_config with is_local false in a statement.
		falseConfig := regexp.MustCompile(`(?is)set_config\s*\([^)]*,\s*false\s*\)`)
		fset := token.NewFileSet()
		for _, file := range packageFiles(t, fset) {
			ast.Inspect(file, func(node ast.Node) bool {
				literal, ok := node.(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					return true
				}
				text, err := strconv.Unquote(literal.Value)
				if err != nil {
					t.Fatal(err)
				}
				if falseConfig.MatchString(text) {
					t.Errorf("%s holds set_config with is_local false", fset.Position(literal.Pos()))
				}
				for _, statement := range strings.Split(text, ";") {
					if len(strings.Fields(statement)) < 2 {
						continue // a word such as "set" is a token the guard compares, not a statement
					}
					if name, ok := sessionSetting(statement); ok {
						t.Errorf("%s holds a session-scoped SET of %s", fset.Position(literal.Pos()), name)
					}
				}
				return true
			})
		}
	})

	t.Run("ST-SUP-RP04-LC03-T03 A transaction whose tenant setting changed does not commit", func(t *testing.T) {
		env := newPG(t)
		ctx := context.Background()
		tx, err := Begin(ctx, env.runtimeDB, mustTenant(t, "tenant-a"), nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, "SELECT set_config('app.tenant', 'tenant-b', true)"); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO items (tenant_id, note) VALUES ('tenant-b', 'changed')"); err != nil {
			t.Fatal(err)
		}
		err = Commit(ctx, tx, mustTenant(t, "tenant-a"))
		if !errors.Is(err, ErrTenantChanged) || !errors.Is(err, ErrNotCommitted) {
			t.Fatalf("Commit after the tenant setting changed returned %v, want ErrTenantChanged, marked as not committed", err)
		}
		if strings.Contains(err.Error(), "tenant-a") || strings.Contains(err.Error(), "tenant-b") {
			t.Fatalf("the refusal names a tenant value: %v", err)
		}
		if err := tx.Commit(); !errors.Is(err, sql.ErrTxDone) {
			t.Fatalf("the transaction is not ended after the refusal: %v", err)
		}
		var rows int
		if err := env.db.QueryRowContext(ctx, "SELECT count(*) FROM items").Scan(&rows); err != nil || rows != 0 {
			t.Fatalf("items holds %d rows (%v) after the refused commit, want none", rows, err)
		}
	})

	t.Run("ST-SUP-RP04-LC03-T04 A connection that comes back with a tenant set is closed", func(t *testing.T) {
		env := newPG(t)
		ctx := context.Background()
		db, err := Open(ctx, Config{DSN: env.dsn(env.runtime)})
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		db.SetMaxOpenConns(1)
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var before int
		if err := conn.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&before); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.ExecContext(ctx, "SELECT set_config('app.tenant', 'tenant-a', false)"); err != nil {
			t.Fatal(err)
		}
		_ = conn.Close() // the connection goes back to the pool with app.tenant set
		tx, err := Begin(ctx, db, mustTenant(t, "tenant-b"), nil)
		if err != nil {
			t.Fatal(err)
		}
		var during int
		if err := tx.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&during); err != nil {
			t.Fatal(err)
		}
		if err := Commit(ctx, tx, mustTenant(t, "tenant-b")); err != nil {
			t.Fatal(err)
		}
		if during == before {
			t.Fatalf("the next transaction ran on the connection %d, which came back with app.tenant set", before)
		}
		var after int
		var value sql.NullString
		if err := db.QueryRowContext(ctx, "SELECT pg_backend_pid(), current_setting('app.tenant', true)").Scan(&after, &value); err != nil {
			t.Fatal(err)
		}
		if value.String != "" {
			t.Fatalf("the open connection %d holds app.tenant = %q outside a transaction", after, value.String)
		}
		var alive int
		for i := 0; i < 50; i++ {
			if err := env.db.QueryRowContext(ctx, "SELECT count(*) FROM pg_stat_activity WHERE pid = $1", before).Scan(&alive); err != nil {
				t.Fatal(err)
			}
			if alive == 0 {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if alive != 0 {
			t.Fatalf("the connection %d is still open, want it closed", before)
		}
	})
}

func TestTenantPoolerGuard(t *testing.T) {
	t.Run("ST-SUP-RP04-LC04-T01 The pooler guard refuses a session-scoped option, startup parameter or statement", func(t *testing.T) {
		const dsn = "postgres://runtime@127.0.0.1:5432/service?sslmode=disable"
		if err := Guard(Config{DSN: dsn}); err != nil {
			t.Fatalf("the guard refused a clean configuration: %v", err)
		}
		for name, cfg := range map[string]Config{
			"connection option": {DSN: dsn + "&options=-c%20app.tenant%3DA"},
			"startup parameter": {DSN: dsn, Params: map[string]string{"app.tenant": "A"}},
			"after-connect SET": {DSN: dsn, AfterConnect: []string{"SET app.tenant = 'A'"}},
		} {
			err := Guard(cfg)
			if err == nil {
				t.Errorf("the guard accepted the %s", name)
				continue
			}
			if !strings.Contains(err.Error(), "app.tenant") {
				t.Errorf("the refusal of the %s does not name the setting: %v", name, err)
			}
		}
		for _, statement := range []string{
			"SET SESSION app.tenant TO 'A'",
			"SELECT set_config('app.tenant', 'A', false)",
			"RESET app.tenant",
			"/* comment */ SET app.tenant = 'A'",
			"SELECT 1; SET app.tenant = 'A'",
			"DO $$ BEGIN PERFORM set_config('app.tenant', 'A', false); END $$",
			"ALTER ROLE CURRENT_USER SET app.tenant = 'A'",
			"SET \"app\".\"tenant\" = 'A'",
		} {
			if err := Guard(Config{DSN: dsn, AfterConnect: []string{statement}}); err == nil ||
				!strings.Contains(err.Error(), "after-connect statement") {
				t.Errorf("the guard did not refuse the after-connect statement %q: %v", statement, err)
			}
		}
	})

	t.Run("ST-SUP-RP04-LC04-T02 The pooler guard checks that the first connection carries no tenant outside a transaction", func(t *testing.T) {
		env := newPG(t)
		ctx := context.Background()
		defaulted := env.role(t, "defaulted", "")
		adminExec(t, env.admin, "ALTER ROLE "+defaulted+" SET app.tenant = 'tenant-a'")
		db, err := Open(ctx, Config{DSN: env.dsn(defaulted)})
		if err == nil {
			_ = db.Close()
			t.Fatal("the guard accepted a connection whose role default sets app.tenant")
		}
		if !strings.Contains(err.Error(), "app.tenant") {
			t.Errorf("the refusal does not name the setting: %v", err)
		}
		clean, err := Open(ctx, Config{DSN: env.dsn(env.runtime)})
		if err != nil {
			t.Fatalf("the guard refused a clean connection: %v", err)
		}
		_ = clean.Close()
	})
}

// sessionSetting reports whether statement starts with a session-scoped SET or RESET or calls set_config with
// is_local not true, and returns the name of the setting. The test scans the statement log and the package source
// with it; the guard itself parses no statement.
func sessionSetting(statement string) (string, bool) {
	words := strings.Fields(strings.ToLower(strings.TrimSpace(statement)))
	if len(words) == 0 {
		return "", false
	}
	switch words[0] {
	case "set":
		rest := words[1:]
		if len(rest) > 0 && rest[0] == "local" {
			return "", false
		}
		if len(rest) > 0 && rest[0] == "session" {
			rest = rest[1:]
		}
		if len(rest) == 0 {
			return "(unnamed)", true
		}
		name, _, _ := strings.Cut(rest[0], "=")
		return name, true
	case "reset":
		if len(words) > 1 {
			return strings.TrimSuffix(words[1], ";"), true
		}
		return "(unnamed)", true
	}
	compact := strings.Join(words, "")
	for {
		at := strings.Index(compact, "set_config(")
		if at < 0 {
			return "", false
		}
		compact = compact[at+len("set_config("):]
		end := strings.Index(compact, ")")
		if end < 0 {
			end = len(compact)
		}
		args := strings.Split(compact[:end], ",")
		if len(args) != 3 || args[2] != "true" {
			return strings.Trim(args[0], "'\""), true
		}
	}
}
