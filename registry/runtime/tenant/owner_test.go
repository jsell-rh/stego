package tenant

import (
	"context"
	"database/sql"
	"net"
	"reflect"
	"runtime"
	"strings"
	"sync"
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
		// A member of the outbox relay role could take it and change every tenant's messages.
		relayMember := env.role(t, "relaymember", "INHERIT")
		adminExec(t, env.admin, `DO $do$ BEGIN CREATE ROLE stego_outbox_relay NOLOGIN;
EXCEPTION WHEN duplicate_object OR unique_violation THEN NULL; END $do$`, "GRANT stego_outbox_relay TO "+relayMember)
		for role, want := range map[string]string{
			bypassMember: "holds BYPASSRLS through its membership of " + bypassGroup,
			superMember:  "is a superuser through its membership of " + superGroup,
			relayMember:  "is a member of the outbox relay or hook roles stego_outbox_relay",
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

func TestOpened(t *testing.T) {
	t.Run("ST-SUP-RP04-LC08-T01 Opened is true only for an open pool of Open or OpenOwner", func(t *testing.T) {
		env := newPG(t)
		ctx := context.Background()
		db, err := Open(ctx, Config{DSN: env.dsn(env.runtime)})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		owner, err := OpenOwner(ctx, MigrationJob, Config{DSN: env.dsn(env.owner)})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = owner.Close() })
		plain, err := sql.Open("pgx", env.dsn(env.runtime))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = plain.Close() })
		// A copy of the pool value, made by reflection so that vet does not refuse the copy of its lock.
		copied := reflect.New(reflect.TypeOf(db).Elem())
		copied.Elem().Set(reflect.ValueOf(db).Elem())
		for name, c := range map[string]struct {
			db   *sql.DB
			want bool
		}{
			"the pool of Open":        {db, true},
			"the pool of OpenOwner":   {owner, true},
			"a pool of sql.Open":      {plain, false},
			"a copy of the Open pool": {copied.Interface().(*sql.DB), false},
			"no pool":                 {nil, false},
		} {
			if got := Opened(c.db); got != c.want {
				t.Errorf("Opened of %s is %v, want %v", name, got, c.want)
			}
		}
		closed, err := Open(ctx, Config{DSN: env.dsn(env.runtime)})
		if err != nil {
			t.Fatal(err)
		}
		if !Opened(closed) {
			t.Fatal("Opened of a new pool of Open is false")
		}
		_ = closed.Close()
		if Opened(closed) {
			t.Error("Opened of a closed pool of Open is true")
		}

		// Earlier pools are collected; many new pools, of sql.Open and of Open, are checked by concurrent callers
		// with no false positive.
		for i := 0; i < 5; i++ {
			old, err := Open(ctx, Config{DSN: env.dsn(env.runtime)})
			if err != nil {
				t.Fatal(err)
			}
			_ = old.Close()
		}
		for i := 0; i < 5; i++ {
			runtime.GC()
		}
		var fresh []*sql.DB
		for i := 0; i < 3; i++ {
			p, err := Open(ctx, Config{DSN: env.dsn(env.runtime)})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = p.Close() })
			fresh = append(fresh, p)
		}
		var others []*sql.DB
		for i := 0; i < 200; i++ {
			p, err := sql.Open("pgx", env.dsn(env.runtime))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = p.Close() })
			others = append(others, p)
		}
		var wrong atomic.Int64
		var wg sync.WaitGroup
		for g := 0; g < 8; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for _, p := range others {
					if Opened(p) {
						wrong.Add(1)
					}
				}
				for _, p := range fresh {
					if !Opened(p) {
						wrong.Add(1)
					}
				}
			}()
		}
		wg.Wait()
		if n := wrong.Load(); n != 0 {
			t.Errorf("%d answers of Opened were wrong under concurrent callers", n)
		}
	})
}
