package tenant

import (
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// countingServer accepts TCP connections and closes them at once, and counts them.
func countingServer(t *testing.T) (string, *atomic.Int64) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	var count atomic.Int64
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			count.Add(1)
			_ = conn.Close()
		}
	}()
	return listener.Addr().String(), &count
}

func TestTenantConnectors(t *testing.T) {
	t.Run("ST-SUP-RP04-LC06-T01 The owner connector opens only in the migration job", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		address, count := countingServer(t)
		cfg := Config{DSN: "postgres://owner:secret@" + address + "/service?sslmode=disable&connect_timeout=2"}
		for _, mode := range []string{"api", "worker", "controller", "all"} {
			db, err := OpenOwner(ctx, mode, cfg)
			if err == nil {
				_ = db.Close()
				t.Fatalf("the owner connector opened in the mode %s", mode)
			}
			if !strings.Contains(err.Error(), `"`+mode+`"`) {
				t.Errorf("the refusal in the mode %s does not name the mode: %v", mode, err)
			}
		}
		if got := count.Load(); got != 0 {
			t.Fatalf("the refused calls opened %d connections, want none", got)
		}
		// In the migration job the connector connects: to the counting server, which closes the connection, and
		// to the local PostgreSQL server.
		if db, err := OpenOwner(ctx, MigrationJob, cfg); err == nil {
			_ = db.Close()
		}
		if count.Load() == 0 {
			t.Fatal("the owner connector made no connection in the migration job")
		}
		env := newPG(t)
		db, err := OpenOwner(ctx, MigrationJob, Config{DSN: env.dsn(env.owner)})
		if err != nil {
			t.Fatalf("the owner connector did not open in the migration job: %v", err)
		}
		_ = db.Close()
	})

	t.Run("ST-SUP-RP04-LC06-T02 The runtime connector refuses a role that owns a table", func(t *testing.T) {
		env := newPG(t)
		ctx := context.Background()
		owning := env.role(t, "owning", "")
		adminExec(t, env.db, "CREATE TABLE owned (id int)", "ALTER TABLE owned OWNER TO "+owning)
		db, err := Open(ctx, Config{DSN: env.dsn(owning)})
		if err == nil {
			_ = db.Close()
			t.Fatal("the runtime connector started as a role that owns a table")
		}
		if !strings.Contains(err.Error(), owning) || !strings.Contains(err.Error(), "public.owned") {
			t.Fatalf("the startup error does not name the role %s and the table public.owned: %v", owning, err)
		}
	})

	t.Run("ST-SUP-RP04-LC06-T03 The runtime connector refuses a role with BYPASSRLS", func(t *testing.T) {
		env := newPG(t)
		ctx := context.Background()
		bypassing := env.role(t, "bypassing", "BYPASSRLS")
		db, err := Open(ctx, Config{DSN: env.dsn(bypassing)})
		if err == nil {
			_ = db.Close()
			t.Fatal("the runtime connector started as a role with BYPASSRLS")
		}
		if !strings.Contains(err.Error(), "BYPASSRLS") {
			t.Errorf("the startup error does not name BYPASSRLS: %v", err)
		}
		// A superuser bypasses row-level security as BYPASSRLS does.
		superuser := env.role(t, "superuser", "SUPERUSER")
		if db, err := Open(ctx, Config{DSN: env.dsn(superuser)}); err == nil {
			_ = db.Close()
			t.Error("the runtime connector started as a superuser")
		} else if !strings.Contains(err.Error(), "is a superuser") {
			t.Errorf("the startup error for the superuser does not say so: %v", err)
		}
		plain, err := Open(ctx, Config{DSN: env.dsn(env.runtime)})
		if err != nil {
			t.Fatalf("the runtime connector did not start as the plain runtime role: %v", err)
		}
		_ = plain.Close()
	})
	t.Run("ST-SUP-RP04-LC06-T04 The runtime connector refuses a role that gets superuser or BYPASSRLS through membership", func(t *testing.T) {
		env := newPG(t)
		ctx := context.Background()
		bypassGroup := env.role(t, "bypassgroup", "NOLOGIN BYPASSRLS")
		bypassMember := env.role(t, "bypassmember", "INHERIT")
		superGroup := env.role(t, "supergroup", "NOLOGIN SUPERUSER")
		superMember := env.role(t, "supermember", "INHERIT")
		adminExec(t, env.admin, "GRANT "+bypassGroup+" TO "+bypassMember, "GRANT "+superGroup+" TO "+superMember)
		for role, want := range map[string]string{
			bypassMember: "holds BYPASSRLS through its membership of " + bypassGroup,
			superMember:  "is a superuser through its membership of " + superGroup,
		} {
			db, err := Open(ctx, Config{DSN: env.dsn(role)})
			if err == nil {
				_ = db.Close()
				t.Errorf("the runtime connector started as the role %s", role)
				continue
			}
			if !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), role) {
				t.Errorf("the startup error for %s does not say %q: %v", role, want, err)
			}
		}
		plain, err := Open(ctx, Config{DSN: env.dsn(env.runtime)})
		if err != nil {
			t.Fatalf("the runtime connector did not start as the plain runtime role: %v", err)
		}
		_ = plain.Close()
	})
}
