package tenant

import (
	"context"
	"database/sql"
	"go/ast"
	"go/token"
	"regexp"
	"strconv"
	"strings"
	"testing"
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
		} {
			if err := Guard(Config{DSN: dsn, AfterConnect: []string{statement}}); err == nil ||
				!strings.Contains(err.Error(), "app.tenant") {
				t.Errorf("the guard did not refuse the after-connect statement %q with its setting: %v", statement, err)
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
