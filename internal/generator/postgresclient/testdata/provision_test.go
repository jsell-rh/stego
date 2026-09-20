package postgres

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"go.opentelemetry.io/otel"
	metricSDK "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	traceSDK "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestDatabaseCredentialPreparationValidation(t *testing.T) {
	value := DatabaseCredentials{ServerIdentity: strings.Repeat("c", 64), Password: strings.Repeat("d", 64)}
	for _, format := range []string{"%s", "%v", "%+v", "%#v", "%q"} {
		for _, subject := range []any{value, &value, struct{ Value DatabaseCredentials }{value}, map[string]DatabaseCredentials{"value": value}} {
			text := fmt.Sprintf(format, subject)
			if strings.Contains(text, value.Password) || strings.Contains(text, value.ServerIdentity) {
				t.Fatal("credential formatting disclosed its fields")
			}
		}
	}
	if data, err := json.Marshal(value); err == nil || len(data) != 0 {
		t.Fatal("credentials were serialized without protected storage")
	}
	for _, ctx := range []context.Context{nil, context.Background()} {
		result, err := PrepareDatabaseCredentials(ctx, Options{}, DatabaseKey{})
		if err == nil || result != (DatabaseCredentials{}) {
			t.Fatal("invalid preparation returned credentials")
		}
	}
}

func TestDatabaseNamesAndValidation(t *testing.T) {
	a, err := DatabaseNames(DatabaseKey{Scope: "installation", Resource: "one'; DROP DATABASE postgres; --"})
	if err != nil || !strings.HasPrefix(a.Database, "stego_") || a.Owner == a.User || len(a.Owner) > 63 || strings.ContainsAny(a.Database, "'; ") {
		t.Fatal("unsafe database identity", err)
	}
	b, _ := DatabaseNames(DatabaseKey{Scope: "other", Resource: "one'; DROP DATABASE postgres; --"})
	if a == b {
		t.Fatal("installation identity was lost")
	}
	for _, key := range []DatabaseKey{{Scope: "", Resource: "one"}, {Scope: "ok", Resource: "\x00"}, {Scope: "ok", Resource: strings.Repeat("a", 257)}} {
		if _, err := DatabaseNames(key); err == nil {
			t.Fatal("invalid key accepted")
		}
	}
	password, err := NewDatabasePassword()
	if err != nil || len(password) != 64 {
		t.Fatal("invalid generated password", err)
	}
	if _, err = EnsureDatabase(nil, Options{}, DatabaseSpec{}); err == nil {
		t.Fatal("nil context accepted")
	}
	if _, err = EnsureDatabase(context.Background(), Options{}, DatabaseSpec{Key: DatabaseKey{"ok", "ok"}, Password: "plain-password", ConnectionLimit: 32}); err == nil {
		t.Fatal("weak credential input accepted")
	}
}

// This check needs a dedicated disposable server. Never revoke PUBLIC access
// on a shared test server or a server that has application databases.
func TestDatabaseProvisioningLifecycle(t *testing.T) {
	if os.Getenv("STEGO_REQUIRE_PROVISIONING_POSTGRES") != "1" {
		t.Skip("requires a dedicated provisioning test server")
	}
	o := sqlFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	connect := func(options Options) *pgx.Conn {
		t.Helper()
		c, err := adminConfig(options)
		if err != nil {
			t.Fatal(err)
		}
		conn, err := pgx.ConnectConfig(ctx, c)
		if err != nil {
			t.Fatal("fixture connection", safeError(ctx, "connect", err))
		}
		t.Cleanup(func() { _ = conn.Close(context.Background()) })
		return conn
	}
	bootstrap := connect(o)
	exec := func(conn *pgx.Conn, query string, args ...any) {
		t.Helper()
		if _, err := conn.Exec(ctx, query, args...); err != nil {
			t.Fatal("fixture SQL", safeError(ctx, "fixture", err))
		}
	}
	var count int
	if err := bootstrap.QueryRow(ctx, "SELECT count(*) FROM pg_catalog.pg_database WHERE datname NOT IN ('postgres','template0','template1')").Scan(&count); err != nil || count != 0 {
		t.Fatal("the provisioning test requires an empty dedicated server", err)
	}
	password, err := NewDatabasePassword()
	if err != nil {
		t.Fatal(err)
	}
	admin := "stego_fixture_" + password[:12]
	ledger := admin + "_ledger"
	exec(bootstrap, "CREATE ROLE "+quoted(admin)+" LOGIN NOSUPERUSER CREATEDB CREATEROLE PASSWORD '"+password+"'")
	exec(bootstrap, "CREATE DATABASE "+quoted(ledger)+" OWNER "+quoted(admin))
	cleanupNames := []DatabaseIdentity{}
	t.Cleanup(func() {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		config, _ := adminConfig(o)
		conn, err := pgx.ConnectConfig(cleanCtx, config)
		if err != nil {
			t.Error("cleanup connection failed")
			return
		}
		defer conn.Close(cleanCtx)
		for _, n := range cleanupNames {
			for _, q := range []string{"DROP DATABASE IF EXISTS " + quoted(n.Database) + " WITH (FORCE)", "DROP ROLE IF EXISTS " + quoted(n.User), "DROP ROLE IF EXISTS " + quoted(n.Owner)} {
				if _, err := conn.Exec(cleanCtx, q); err != nil {
					t.Error("owned fixture cleanup failed", safeError(cleanCtx, "cleanup", err))
				}
			}
		}
		for _, q := range []string{"DROP DATABASE " + quoted(ledger) + " WITH (FORCE)", "DROP ROLE " + quoted(admin), "GRANT CONNECT ON DATABASE postgres,template1 TO PUBLIC"} {
			if _, err := conn.Exec(cleanCtx, q); err != nil {
				t.Error("fixture cleanup failed", safeError(cleanCtx, "cleanup", err))
			}
		}
	})
	provisioner := o
	provisioner.User, provisioner.Database, provisioner.Password = admin, ledger, password
	adminConn := connect(provisioner)

	server, err := DatabaseServerIdentity(ctx, provisioner)
	if err != nil || len(server) != 64 {
		t.Fatal("server identity", err)
	}
	provisioner.ServerIdentity = server
	if again, err := DatabaseServerIdentity(ctx, provisioner); err != nil || again != server {
		t.Fatal("server identity changed", err)
	}
	wrong := provisioner
	wrong.ServerIdentity = strings.Repeat("0", 64)
	if _, err := DatabaseServerIdentity(ctx, wrong); !errors.Is(err, ErrDatabaseServer) {
		t.Fatal("changed server accepted", err)
	}
	// A restored server without its identity cannot prepare, create, or delete resources.
	exec(adminConn, "ALTER TABLE stego_provisioning.server_identity RENAME TO saved_server_identity")
	candidatePassword, _ := NewDatabasePassword()
	candidate := DatabaseSpec{Key: DatabaseKey{"binding-test", "missing"}, Password: candidatePassword, ConnectionLimit: 8}
	if _, err := EnsureDatabase(ctx, provisioner, candidate); !errors.Is(err, ErrDatabaseServer) {
		t.Fatal("missing server marker allowed creation", err)
	}
	if value, err := PrepareDatabaseCredentials(ctx, provisioner, candidate.Key); !errors.Is(err, ErrDatabaseServer) || value != (DatabaseCredentials{}) {
		t.Fatal("missing marker allowed credential preparation", err)
	}
	if err := DeleteDatabase(ctx, provisioner, candidate.Key); !errors.Is(err, ErrDatabaseServer) {
		t.Fatal("missing server marker allowed deletion", err)
	}
	var bindingRows int
	if err := adminConn.QueryRow(ctx, "SELECT count(*) FROM stego_provisioning.resources").Scan(&bindingRows); err != nil || bindingRows != 0 {
		t.Fatal("server mismatch changed the ledger", err)
	}
	exec(adminConn, "ALTER TABLE stego_provisioning.saved_server_identity RENAME TO server_identity")
	var super bool
	if err = adminConn.QueryRow(ctx, "SELECT rolsuper FROM pg_catalog.pg_roles WHERE rolname=current_user").Scan(&super); err != nil || super {
		t.Fatal("provisioning account is a superuser", err)
	}
	spec := func(resource string) DatabaseSpec {
		t.Helper()
		pw, e := NewDatabasePassword()
		if e != nil {
			t.Fatal(e)
		}
		s := DatabaseSpec{Key: DatabaseKey{admin, resource}, Password: pw, ConnectionLimit: 8}
		n, _ := DatabaseNames(s.Key)
		cleanupNames = append(cleanupNames, n)
		return s
	}
	unsafe := spec("unsafe-defaults")
	if _, err = EnsureDatabase(ctx, provisioner, unsafe); !errors.Is(err, ErrDatabaseIsolation) {
		var remote *Error
		if errors.As(err, &remote) {
			t.Fatalf("unsafe default check: stage=%s SQLSTATE=%s", remote.Stage, remote.SQLState)
		}
		t.Fatal("PUBLIC connection access was accepted", err)
	}
	unsafeNames, _ := DatabaseNames(unsafe.Key)
	var login bool
	if err = bootstrap.QueryRow(ctx, "SELECT rolcanlogin FROM pg_catalog.pg_roles WHERE rolname=$1", unsafeNames.User).Scan(&login); err != nil || login {
		t.Fatal("unsafe role was enabled", err)
	}
	exec(bootstrap, "REVOKE CONNECT ON DATABASE postgres,template1,"+quoted(ledger)+" FROM PUBLIC")
	if err = DeleteDatabase(ctx, provisioner, unsafe.Key); err != nil {
		t.Fatal("reserved cleanup", err)
	}

	t.Run("credential-preparation", func(t *testing.T) {
		candidate := spec("credential-preparation")
		names, _ := DatabaseNames(candidate.Key)
		reject := func(options Options, key DatabaseKey, want error) {
			t.Helper()
			value, err := PrepareDatabaseCredentials(ctx, options, key)
			if !errors.Is(err, want) || value != (DatabaseCredentials{}) {
				t.Fatal("preparation did not reject its state", err)
			}
		}
		wrong := provisioner
		wrong.ServerIdentity = strings.Repeat("0", 64)
		reject(wrong, candidate.Key, ErrDatabaseServer)
		stopped, cancel := context.WithCancel(ctx)
		cancel()
		if value, err := PrepareDatabaseCredentials(stopped, provisioner, candidate.Key); !errors.Is(err, context.Canceled) || value != (DatabaseCredentials{}) {
			t.Fatal("canceled preparation returned credentials", err)
		}
		held, err := openProvision(ctx, provisioner, candidate.Key)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = held.conn.Close(ctx) }()
		reject(provisioner, candidate.Key, ErrDatabaseBusy)
		other, err := PrepareDatabaseCredentials(ctx, provisioner, DatabaseKey{candidate.Key.Scope, "other-preparation"})
		_ = held.conn.Close(ctx)
		if err != nil || other.ServerIdentity != server || len(other.Password) != 64 {
			t.Fatal("one resource lock blocked another resource", err)
		}
		for _, name := range []string{names.Owner, names.User} {
			exec(bootstrap, "CREATE ROLE "+quoted(name)+" NOLOGIN")
			reject(provisioner, candidate.Key, ErrDatabaseOwnership)
			exec(bootstrap, "DROP ROLE "+quoted(name))
		}
		exec(bootstrap, "CREATE DATABASE "+quoted(names.Database))
		reject(provisioner, candidate.Key, ErrDatabaseOwnership)
		exec(bootstrap, "DROP DATABASE "+quoted(names.Database))
		prepared, err := PrepareDatabaseCredentials(ctx, provisioner, candidate.Key)
		if err != nil || prepared.ServerIdentity != server || len(prepared.Password) != 64 {
			t.Fatal("credential preparation failed", err)
		}
		var absent bool
		err = adminConn.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM pg_catalog.pg_database WHERE datname=$1) AND NOT EXISTS(SELECT 1 FROM pg_catalog.pg_roles WHERE rolname IN ($2,$3)) AND NOT EXISTS(SELECT 1 FROM stego_provisioning.resources WHERE scope=$4 AND resource=$5)`, names.Database, names.Owner, names.User, candidate.Key.Scope, candidate.Key.Resource).Scan(&absent)
		if err != nil || !absent {
			t.Fatal("preparation created SQL resource state", err)
		}
		candidate.Password = prepared.Password
		session, err := openProvision(ctx, provisioner, candidate.Key)
		if err != nil {
			t.Fatal(err)
		}
		_, err = session.reserve(candidate)
		_ = session.conn.Close(ctx)
		if err != nil {
			t.Fatal(err)
		}
		reject(provisioner, candidate.Key, ErrDatabaseCredential)
		if _, err = EnsureDatabase(ctx, provisioner, candidate); err != nil {
			t.Fatal("saved candidate did not provision", err)
		}
		reject(provisioner, candidate.Key, ErrDatabaseCredential)
		if err = DeleteDatabase(ctx, provisioner, candidate.Key); err != nil {
			t.Fatal(err)
		}
		reject(provisioner, candidate.Key, ErrDatabaseDeleted)
	})

	t.Run("managed-schema", func(t *testing.T) {
		managed := spec("browser-schema")
		managed.ManagedSchema = true
		names, err := EnsureDatabase(ctx, provisioner, managed)
		if err != nil {
			t.Fatal("managed database", err)
		}
		runtimeOptions := o
		runtimeOptions.Database, runtimeOptions.User, runtimeOptions.Password = names.Database, names.User, managed.Password
		runtime := connect(runtimeOptions)
		deny := func(query string) {
			t.Helper()
			_, err := runtime.Exec(ctx, query)
			var server *pgconn.PgError
			if !errors.As(err, &server) || server.Code != "42501" {
				t.Fatal("runtime operation was not denied", safeError(ctx, "denial", err))
			}
		}
		deny("CREATE TABLE public.forbidden(value integer)")
		deny("CREATE TEMP TABLE forbidden(value integer)")
		var retained *sql.Conn
		err = WithDatabaseOwner(ctx, provisioner, managed, func(schemaCtx context.Context, conn *sql.Conn, identity DatabaseIdentity) error {
			retained = conn
			if identity != names {
				t.Fatal("schema identity differs")
			}
			if _, ok := schemaCtx.Deadline(); !ok {
				t.Fatal("schema context has no deadline")
			}
			if err := WithDatabaseOwner(schemaCtx, provisioner, managed, func(context.Context, *sql.Conn, DatabaseIdentity) error {
				t.Fatal("concurrent callback ran")
				return nil
			}); !errors.Is(err, ErrDatabaseBusy) {
				t.Fatal("schema lock was not held", err)
			}
			tx, err := conn.BeginTx(schemaCtx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			if _, err = tx.ExecContext(schemaCtx, "CREATE TABLE public.managed_marker(value integer NOT NULL); GRANT SELECT,INSERT,UPDATE,DELETE ON public.managed_marker TO "+quoted(identity.User)); err != nil {
				return err
			}
			return tx.Commit()
		})
		if err != nil {
			t.Fatal("schema setup", err)
		}
		if _, err = retained.ExecContext(ctx, "SELECT 1"); !errors.Is(err, sql.ErrConnDone) {
			t.Fatal("schema connection survived callback", err)
		}
		exec(runtime, "INSERT INTO public.managed_marker VALUES(41); UPDATE public.managed_marker SET value=42")
		var value int
		if err = runtime.QueryRow(ctx, "SELECT value FROM public.managed_marker").Scan(&value); err != nil || value != 42 {
			t.Fatal("runtime data access", err)
		}
		deny("ALTER TABLE public.managed_marker ADD COLUMN forbidden integer")
		deny("TRUNCATE public.managed_marker")
		// The provider must preserve the limited mode on both retries and repair.
		for i := 0; i < 2; i++ {
			if _, err = EnsureDatabase(ctx, provisioner, managed); err != nil {
				t.Fatal("managed retry", err)
			}
		}
		exec(bootstrap, "GRANT TEMPORARY ON DATABASE "+quoted(names.Database)+" TO "+quoted(names.User))
		calls := 0
		callback := func(context.Context, *sql.Conn, DatabaseIdentity) error { calls++; return nil }
		if err = WithDatabaseOwner(ctx, provisioner, managed, callback); !errors.Is(err, ErrDatabaseIsolation) || calls != 0 {
			t.Fatal("unsafe runtime reached schema callback", err)
		}
		if _, err = EnsureDatabase(ctx, provisioner, managed); err != nil {
			t.Fatal("managed repair", err)
		}
		deny("CREATE TEMP TABLE forbidden(value integer)")
		err = WithDatabaseOwner(ctx, provisioner, managed, func(schemaCtx context.Context, conn *sql.Conn, _ DatabaseIdentity) error {
			var owner string
			if err := conn.QueryRowContext(schemaCtx, "SELECT pg_catalog.pg_get_userbyid(relowner) FROM pg_catalog.pg_class WHERE oid='public.managed_marker'::regclass").Scan(&owner); err != nil {
				return err
			}
			if owner != names.Owner {
				t.Fatal("runtime owns managed table")
			}
			return nil
		})
		if err != nil {
			t.Fatal("owner reconnect", err)
		}
		changed := managed
		changed.Password = strings.Repeat("0", 64)
		if err = WithDatabaseOwner(ctx, provisioner, changed, callback); !errors.Is(err, ErrDatabaseCredential) || calls != 0 {
			t.Fatal("changed credential reached schema callback", err)
		}
		changed = managed
		changed.Key.Resource = "absent-schema"
		if err = WithDatabaseOwner(ctx, provisioner, changed, callback); !errors.Is(err, ErrDatabaseOwnership) || calls != 0 {
			t.Fatal("absent database reached schema callback", err)
		}
		wrongServer := provisioner
		wrongServer.ServerIdentity = strings.Repeat("0", 64)
		if err = WithDatabaseOwner(ctx, wrongServer, managed, callback); !errors.Is(err, ErrDatabaseServer) || calls != 0 {
			t.Fatal("changed server reached schema callback", err)
		}
		err = WithDatabaseOwner(ctx, provisioner, managed, func(context.Context, *sql.Conn, DatabaseIdentity) error {
			return errors.New("private-schema-error-" + managed.Password)
		})
		if err == nil || strings.Contains(err.Error(), managed.Password) || strings.Contains(err.Error(), "private-schema") {
			t.Fatal("schema error was not redacted")
		}
		err = WithDatabaseOwner(ctx, provisioner, managed, func(context.Context, *sql.Conn, DatabaseIdentity) error { return context.DeadlineExceeded })
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("callback deadline was lost", err)
		}
		canceled, cancelSchema := context.WithCancel(ctx)
		err = WithDatabaseOwner(canceled, provisioner, managed, func(context.Context, *sql.Conn, DatabaseIdentity) error { cancelSchema(); return nil })
		cancelSchema()
		if !errors.Is(err, context.Canceled) {
			t.Fatal("callback cancellation was lost", err)
		}
		if err = DeleteDatabase(ctx, provisioner, managed.Key); err != nil {
			t.Fatal("managed deletion", err)
		}
		if err = WithDatabaseOwner(ctx, provisioner, managed, callback); !errors.Is(err, ErrDatabaseDeleted) || calls != 0 {
			t.Fatal("deleted database reached schema callback", err)
		}
	})

	a, b := spec("first"), spec("second")
	first, err := EnsureDatabase(ctx, provisioner, a)
	if err != nil {
		t.Fatal("first database", err)
	}
	second, err := EnsureDatabase(ctx, provisioner, b)
	if err != nil {
		t.Fatal("second database", err)
	}
	appOptions := func(s DatabaseSpec, n DatabaseIdentity) Options {
		v := o
		v.Database, v.User, v.Password = n.Database, n.User, s.Password
		return v
	}
	app := connect(appOptions(a, first))
	other := connect(appOptions(b, second))
	exec(app, "CREATE TABLE public.marker(value integer); INSERT INTO public.marker VALUES(42)")
	exec(other, "CREATE TABLE public.marker(value integer); INSERT INTO public.marker VALUES(99)")
	var actualOwner string
	if err = bootstrap.QueryRow(ctx, "SELECT r.rolname FROM pg_catalog.pg_database d JOIN pg_catalog.pg_roles r ON r.oid=d.datdba WHERE d.datname=$1", first.Database).Scan(&actualOwner); err != nil || actualOwner != first.Owner {
		t.Fatal("application login owns the database", err)
	}
	for _, database := range []string{second.Database, ledger, "postgres", "template1"} {
		options := appOptions(a, first)
		options.Database = database
		var value int
		err = ReadRow(ctx, options, "SELECT 1", nil, &value)
		var remote *Error
		if !errors.As(err, &remote) || remote.Stage != "connect" || remote.SQLState != "42501" {
			t.Fatal("cross-database connection was not denied", database, err)
		}
	}
	if _, err = app.Exec(ctx, "CREATE ROLE must_not_exist"); err == nil {
		t.Fatal("application login created a role")
	}
	exec(app, "GRANT CONNECT ON DATABASE "+quoted(first.Database)+" TO "+quoted(second.User))
	var allowed bool
	if err = bootstrap.QueryRow(ctx, "SELECT pg_catalog.has_database_privilege($1,$2,'CONNECT')", second.User, first.Database).Scan(&allowed); err != nil || allowed {
		t.Fatal("application login granted database access", err)
	}
	// A healthy call must not write catalog rows or change the stored verifier.
	// The ledger is in a separate database, so compare its row through its owner.
	var catalogBefore string
	if err = bootstrap.QueryRow(ctx, "SELECT d.xmin::text||':'||r.xmin::text||':'||r.rolpassword FROM pg_catalog.pg_database d,pg_catalog.pg_authid r WHERE d.datname=$1 AND r.rolname=$2", first.Database, first.User).Scan(&catalogBefore); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		got, e := EnsureDatabase(ctx, provisioner, a)
		if e != nil || got != first {
			t.Fatal("stable repeat", e)
		}
	}
	var catalogAfter string
	if err = bootstrap.QueryRow(ctx, "SELECT d.xmin::text||':'||r.xmin::text||':'||r.rolpassword FROM pg_catalog.pg_database d,pg_catalog.pg_authid r WHERE d.datname=$1 AND r.rolname=$2", first.Database, first.User).Scan(&catalogAfter); err != nil || catalogAfter != catalogBefore {
		t.Fatal("healthy reconciliation changed PostgreSQL catalogs", err)
	}
	changed := a
	changed.Password = strings.Repeat("a", 64)
	if _, err = EnsureDatabase(ctx, provisioner, changed); !errors.Is(err, ErrDatabaseCredential) {
		t.Fatal("missing durable credential was replaced", err)
	}
	// Repair a grant added to this owned database, without altering the other one.
	exec(bootstrap, "GRANT CONNECT ON DATABASE "+quoted(first.Database)+" TO "+quoted(second.User))
	if _, err = EnsureDatabase(ctx, provisioner, a); err != nil {
		t.Fatal("owned ACL repair", err)
	}
	if err = bootstrap.QueryRow(ctx, "SELECT pg_catalog.has_database_privilege($1,$2,'CONNECT')", second.User, first.Database).Scan(&allowed); err != nil || allowed {
		t.Fatal("foreign access survived repair", err)
	}
	if err = app.QueryRow(ctx, "SELECT value FROM public.marker").Scan(&count); err != nil || count != 42 {
		t.Fatal("repair lost data", err)
	}

	// Repair a credential change without replacing stored data.
	replacement, e := NewDatabasePassword()
	if e != nil {
		t.Fatal(e)
	}
	exec(bootstrap, "ALTER ROLE "+quoted(first.User)+" PASSWORD '"+replacement+"'")
	if _, err = EnsureDatabase(ctx, provisioner, a); err != nil {
		t.Fatal("credential repair", err)
	}
	if err = ReadRow(ctx, appOptions(a, first), "SELECT value FROM public.marker", nil, &count); err != nil || count != 42 {
		t.Fatal("credential repair lost access or data", err)
	}
	// The existing application connection fills a one-connection limit. The
	// final login check must fail and restore NOLOGIN on the locked session.
	limited := a
	limited.ConnectionLimit = 1
	if _, err = EnsureDatabase(ctx, provisioner, limited); err == nil {
		t.Fatal("activation passed without a successful login check")
	}
	if err = bootstrap.QueryRow(ctx, "SELECT rolcanlogin FROM pg_catalog.pg_roles WHERE rolname=$1", first.User).Scan(&login); err != nil || login {
		t.Fatal("failed activation left login enabled", err)
	}
	if _, err = EnsureDatabase(ctx, provisioner, a); err != nil {
		t.Fatal("failed activation did not recover", err)
	}
	// Keep owned sessions in both the owned database and another database.
	// The owner role can also have a session after an operator enables login.
	exec(bootstrap, "ALTER ROLE "+quoted(first.Owner)+" LOGIN PASSWORD '"+a.Password+"'")
	ownerOptions := appOptions(a, first)
	ownerOptions.User = first.Owner
	ownerSession := connect(ownerOptions)
	exec(bootstrap, "GRANT CONNECT ON DATABASE postgres TO "+quoted(first.User))
	outsideOptions := appOptions(a, first)
	outsideOptions.Database = "postgres"
	outsideSession := connect(outsideOptions)
	if _, err = EnsureDatabase(ctx, provisioner, a); !errors.Is(err, ErrDatabaseIsolation) {
		t.Fatal("isolation drift accepted", err)
	}
	if err = bootstrap.QueryRow(ctx, "SELECT rolcanlogin FROM pg_catalog.pg_roles WHERE rolname=$1", first.User).Scan(&login); err != nil || login {
		t.Fatal("isolation drift left login enabled", err)
	}
	for _, connection := range []*pgx.Conn{app, ownerSession, outsideSession} {
		if err := connection.Ping(ctx); err == nil {
			t.Fatal("an owned session survived isolation failure")
		}
	}
	if err = other.QueryRow(ctx, "SELECT value FROM public.marker").Scan(&count); err != nil || count != 99 {
		t.Fatal("quarantine stopped an unrelated session", err)
	}
	if err = adminConn.Ping(ctx); err != nil {
		t.Fatal("quarantine stopped the administrator", err)
	}
	exec(bootstrap, "REVOKE CONNECT ON DATABASE postgres FROM "+quoted(first.User))
	if _, err = EnsureDatabase(ctx, provisioner, a); err != nil {
		t.Fatal("isolation recovery", err)
	}
	app = connect(appOptions(a, first))

	for _, fault := range []struct {
		apply, restore string
		ownerLogin     bool
	}{
		{"ALTER ROLE " + quoted(first.User) + " CREATEDB", "ALTER ROLE " + quoted(first.User) + " NOCREATEDB", false},
		{"GRANT pg_read_all_data TO " + quoted(first.User), "REVOKE pg_read_all_data FROM " + quoted(first.User), false},
		{"ALTER ROLE " + quoted(first.Owner) + " LOGIN", "ALTER ROLE " + quoted(first.Owner) + " NOLOGIN", true},
	} {
		exec(bootstrap, fault.apply)
		var ownerConnection *pgx.Conn
		if fault.ownerLogin {
			ownerConnection = connect(ownerOptions)
		}
		if _, err = EnsureDatabase(ctx, provisioner, a); !errors.Is(err, ErrDatabaseIsolation) {
			t.Fatal("unsafe role was not quarantined", err)
		}
		if err = app.Ping(ctx); err == nil {
			t.Fatal("unsafe role retained an open session")
		}
		if ownerConnection != nil && ownerConnection.Ping(ctx) == nil {
			t.Fatal("owner login retained an open session")
		}
		if err = other.Ping(ctx); err != nil {
			t.Fatal("role quarantine stopped another resource", err)
		}
		exec(bootstrap, fault.restore)
		if _, err = EnsureDatabase(ctx, provisioner, a); err != nil {
			t.Fatal("role quarantine did not recover", err)
		}
		app = connect(appOptions(a, first))
	}

	// The server can deny signals. Retain NOLOGIN, return the safe error, and
	// retry only after the operator restores permission. Never report success.
	ledgerOptions := o
	ledgerOptions.Database = ledger
	ledgerAdmin := connect(ledgerOptions)
	const restoreSignal = "GRANT EXECUTE ON FUNCTION pg_catalog.pg_terminate_backend(integer,bigint) TO PUBLIC"
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := ledgerAdmin.Exec(cleanup, restoreSignal); err != nil {
			t.Error("signal permission restore failed", safeError(cleanup, "fixture", err))
		}
	})
	exec(ledgerAdmin, "REVOKE EXECUTE ON FUNCTION pg_catalog.pg_terminate_backend(integer,bigint) FROM PUBLIC")
	var signalAllowed bool
	if err = ledgerAdmin.QueryRow(ctx, "SELECT pg_catalog.has_function_privilege($1,'pg_catalog.pg_terminate_backend(integer,bigint)','EXECUTE')", admin).Scan(&signalAllowed); err != nil || signalAllowed {
		t.Fatal("signal denial fixture did not remove permission", err)
	}
	var signalResult bool
	err = adminConn.QueryRow(ctx, "SELECT pg_catalog.pg_terminate_backend($1,100)", app.PgConn().PID()).Scan(&signalResult)
	var signalError *pgconn.PgError
	if !errors.As(err, &signalError) || signalError.Code != "42501" {
		t.Fatalf("signal denial direct call: error=%t SQLSTATE=%s stopped=%t", err != nil, safeError(ctx, "fixture", err).(*Error).SQLState, signalResult)
	}
	exec(bootstrap, "ALTER ROLE "+quoted(first.User)+" CREATEDB")
	_, err = EnsureDatabase(ctx, provisioner, a)
	var denied *Error
	if !errors.Is(err, ErrDatabaseIsolation) || !errors.As(err, &denied) || denied.Stage != "isolation-sessions" || denied.SQLState != "42501" {
		t.Fatal("denied session termination was not reported", err)
	}
	if err = app.Ping(ctx); err != nil {
		t.Fatal("denied signal fixture did not retain its session", err)
	}
	if err = bootstrap.QueryRow(ctx, "SELECT rolcanlogin FROM pg_catalog.pg_roles WHERE rolname=$1", first.User).Scan(&login); err != nil || login {
		t.Fatal("denied signal restored login", err)
	}
	exec(ledgerAdmin, restoreSignal)
	if _, err = EnsureDatabase(ctx, provisioner, a); !errors.Is(err, ErrDatabaseIsolation) {
		t.Fatal("unsafe role recovered before operator repair", err)
	}
	if err = app.Ping(ctx); err == nil {
		t.Fatal("session termination did not resume")
	}
	exec(bootstrap, "ALTER ROLE "+quoted(first.User)+" NOCREATEDB")
	if _, err = EnsureDatabase(ctx, provisioner, a); err != nil {
		t.Fatal("signal recovery failed", err)
	}
	app = connect(appOptions(a, first))

	// Use two connections and a one-session bound to test bounded progress.
	// This tests the same code without a large connection or stress workload.
	extra := connect(appOptions(a, first))
	quarantine, err := openProvision(ctx, provisioner, a.Key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = quarantine.conn.Close(cleanup)
	})
	retained, found, err := quarantine.record()
	if err != nil || !found {
		t.Fatal("quarantine ledger is absent", err)
	}
	if err = quarantine.quarantine(retained, 0); !errors.Is(err, ErrDatabaseIsolation) {
		t.Fatal("invalid session bound accepted", err)
	}
	if err = app.Ping(ctx); err != nil {
		t.Fatal("invalid bound changed a session", err)
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	quarantine.ctx = canceled
	if err = quarantine.quarantine(retained, 1); !errors.Is(err, context.Canceled) {
		t.Fatal("quarantine ignored cancellation", err)
	}
	_ = quarantine.conn.Close(ctx)
	if err = app.Ping(ctx); err != nil {
		t.Fatal("canceled quarantine changed a session", err)
	}
	quarantine, err = openProvision(ctx, provisioner, a.Key)
	if err != nil {
		t.Fatal("quarantine retry could not open a new session", err)
	}
	if err = quarantine.quarantine(retained, 1); !errors.Is(err, ErrDatabaseIsolation) {
		t.Fatal("session bound was not enforced", err)
	}
	if err = bootstrap.QueryRow(ctx, "SELECT count(*) FROM pg_catalog.pg_stat_activity WHERE usesysid IN ($1,$2)", retained.login, retained.owner).Scan(&count); err != nil || count != 1 {
		t.Fatal("bounded quarantine did not make exact progress", err)
	}
	if err = other.Ping(ctx); err != nil {
		t.Fatal("bounded quarantine stopped another resource", err)
	}
	if err = quarantine.quarantine(retained, 128); err != nil {
		t.Fatal("bounded quarantine did not finish", err)
	}
	_ = quarantine.conn.Close(ctx)
	for _, connection := range []*pgx.Conn{app, extra} {
		if err = connection.Ping(ctx); err == nil {
			t.Fatal("bounded quarantine retained an owned session")
		}
	}
	if _, err = EnsureDatabase(ctx, provisioner, a); err != nil {
		t.Fatal("bounded quarantine did not recover", err)
	}
	app = connect(appOptions(a, first))
	if err = app.QueryRow(ctx, "SELECT value FROM public.marker").Scan(&count); err != nil || count != 42 {
		t.Fatal("quarantine changed stored application data", err)
	}
	t.Log("Owned login and owner sessions stopped across databases; unrelated sessions survived; denied signals, cancellation, bounded progress, and recovery passed")

	// A dependency outside the owned database must stop deletion. Resume after
	// the operator removes that dependency, including an already dropped login.
	dependent := spec("external-dependency")
	dependentNames, err := EnsureDatabase(ctx, provisioner, dependent)
	if err != nil {
		t.Fatal(err)
	}
	exec(bootstrap, "GRANT CONNECT ON DATABASE "+quoted(second.Database)+" TO "+quoted(dependentNames.Owner))
	if err = DeleteDatabase(ctx, provisioner, dependent.Key); err == nil {
		t.Fatal("deletion removed a foreign dependency")
	}
	if _, err = EnsureDatabase(ctx, provisioner, dependent); !errors.Is(err, ErrDatabaseDeleted) {
		t.Fatal("interrupted deletion allowed creation", err)
	}
	if err = bootstrap.QueryRow(ctx, "SELECT pg_catalog.has_database_privilege($1,$2,'CONNECT')", dependentNames.Owner, second.Database).Scan(&allowed); err != nil || !allowed {
		t.Fatal("deletion changed a foreign grant", err)
	}
	if err = other.QueryRow(ctx, "SELECT value FROM public.marker").Scan(&count); err != nil || count != 99 {
		t.Fatal("blocked deletion lost unrelated data", err)
	}
	exec(bootstrap, "REVOKE CONNECT ON DATABASE "+quoted(second.Database)+" FROM "+quoted(dependentNames.Owner))
	if err = DeleteDatabase(ctx, provisioner, dependent.Key); err != nil {
		t.Fatal("partial deletion did not resume", err)
	}

	// PostgreSQL retains datconnlimit=-2 if a drop stops after invalidation.
	// Reproduce that catalog state on this dedicated server. This does not
	// simulate the timing of a physical crash or a canceled checkpoint.
	t.Run("invalid-database-drop", func(t *testing.T) {
		for _, mode := range []string{"owned", "foreign-owner", "replaced-database"} {
			t.Run(mode, func(t *testing.T) {
				candidate := spec("invalid-drop-" + mode)
				names, err := EnsureDatabase(ctx, provisioner, candidate)
				if err != nil {
					t.Fatal("create invalid-drop fixture", err)
				}
				if mode == "owned" {
					exec(adminConn, "UPDATE stego_provisioning.resources SET state='deleting' WHERE scope=$1 AND resource=$2", candidate.Key.Scope, candidate.Key.Resource)
					exec(bootstrap, "ALTER ROLE "+quoted(names.User)+" NOLOGIN")
					exec(bootstrap, "ALTER ROLE "+quoted(names.Owner)+" NOLOGIN")
				} else if mode == "foreign-owner" {
					exec(bootstrap, "ALTER DATABASE "+quoted(names.Database)+" OWNER TO "+quoted(o.User))
				} else {
					exec(bootstrap, "DROP DATABASE "+quoted(names.Database)+" WITH (FORCE)")
					exec(bootstrap, "CREATE DATABASE "+quoted(names.Database)+" OWNER "+quoted(names.Owner))
					exec(bootstrap, "REVOKE ALL ON DATABASE "+quoted(names.Database)+" FROM PUBLIC")
				}
				exec(bootstrap, "ALTER DATABASE "+quoted(names.Database)+" ALLOW_CONNECTIONS false")
				exec(bootstrap, "UPDATE pg_catalog.pg_database SET datconnlimit=-2 WHERE datname=$1", names.Database)
				var oid uint32
				var limit int
				if err := bootstrap.QueryRow(ctx, "SELECT oid,datconnlimit FROM pg_catalog.pg_database WHERE datname=$1", names.Database).Scan(&oid, &limit); err != nil || limit != -2 {
					t.Fatal("invalid database fixture was not established", err)
				}
				if mode == "owned" {
					if _, err := EnsureDatabase(ctx, provisioner, candidate); !errors.Is(err, ErrDatabaseDeleted) {
						t.Fatal("invalid deleting database was recreated", err)
					}
					// Each call opens a new provider session. No connection state
					// from the interrupted operation is required for recovery.
					if err := DeleteDatabase(ctx, provisioner, candidate.Key); err != nil {
						var remote *Error
						if errors.As(err, &remote) {
							t.Fatalf("invalid database drop did not resume: stage=%s SQLSTATE=%s", remote.Stage, remote.SQLState)
						}
						t.Fatal("invalid database drop did not resume", err)
					}
					var absent bool
					if err := adminConn.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM pg_catalog.pg_database WHERE datname=$1)
					 AND NOT EXISTS(SELECT 1 FROM pg_catalog.pg_roles WHERE rolname IN ($2,$3))
					 AND EXISTS(SELECT 1 FROM stego_provisioning.resources WHERE scope=$4 AND resource=$5 AND state='deleted')`, names.Database, names.Owner, names.User, candidate.Key.Scope, candidate.Key.Resource).Scan(&absent); err != nil || !absent {
						t.Fatal("invalid database cleanup was not complete", err)
					}
					if err := DeleteDatabase(ctx, provisioner, candidate.Key); err != nil {
						t.Fatal("repeated invalid database cleanup failed", err)
					}
				} else {
					if err := DeleteDatabase(ctx, provisioner, candidate.Key); !errors.Is(err, ErrDatabaseOwnership) {
						t.Fatal("foreign invalid database was accepted", err)
					}
					var retained bool
					if err := adminConn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_database WHERE datname=$1 AND oid=$2 AND datconnlimit=-2)
					 AND EXISTS(SELECT 1 FROM pg_catalog.pg_roles WHERE rolname=$3 AND rolcanlogin)
					 AND EXISTS(SELECT 1 FROM stego_provisioning.resources WHERE scope=$4 AND resource=$5 AND state='ready')`, names.Database, oid, names.User, candidate.Key.Scope, candidate.Key.Resource).Scan(&retained); err != nil || !retained {
						t.Fatal("denied cleanup changed a foreign database or recorded state", err)
					}
				}
				var marker int
				if err := other.QueryRow(ctx, "SELECT value FROM public.marker").Scan(&marker); err != nil || marker != 99 {
					t.Fatal("invalid database cleanup changed unrelated data", err)
				}
			})
		}
	})

	// A role with the same name and a different OID is not an owned role.
	swapped := spec("replaced-role")
	reserved, err := openProvision(ctx, provisioner, swapped.Key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = reserved.reserve(swapped); err != nil {
		t.Fatal(err)
	}
	swappedNames := reserved.names
	_ = reserved.conn.Close(ctx)
	exec(bootstrap, "DROP ROLE "+quoted(swappedNames.User)+"; CREATE ROLE "+quoted(swappedNames.User)+" NOLOGIN")
	if _, err = EnsureDatabase(ctx, provisioner, swapped); !errors.Is(err, ErrDatabaseOwnership) {
		t.Fatal("replacement role was adopted", err)
	}
	if err = DeleteDatabase(ctx, provisioner, swapped.Key); !errors.Is(err, ErrDatabaseOwnership) {
		t.Fatal("replacement role was deleted", err)
	}
	if err = bootstrap.QueryRow(ctx, "SELECT count(*) FROM pg_catalog.pg_roles WHERE rolname=$1", swappedNames.User).Scan(&count); err != nil || count != 1 {
		t.Fatal("replacement role disappeared", err)
	}

	resume := spec("interrupted-create")
	session, err := openProvision(ctx, provisioner, resume.Key)
	if err != nil {
		t.Fatal(err)
	}
	record, err := session.reserve(resume)
	if err != nil {
		t.Fatal(err)
	}
	if record.database != nil {
		t.Fatal("reservation already has a database")
	}
	if err = session.exec("create", "CREATE DATABASE "+quoted(session.names.Database)+" OWNER "+quoted(session.names.Owner)+" TEMPLATE template0 ALLOW_CONNECTIONS false"); err != nil {
		t.Fatal(err)
	}
	_ = session.conn.Close(ctx)
	if _, err = EnsureDatabase(ctx, provisioner, resume); err != nil {
		t.Fatal("lost CREATE result did not recover", err)
	}
	if err = DeleteDatabase(ctx, provisioner, resume.Key); err != nil {
		t.Fatal(err)
	}

	locked, err := openProvision(ctx, provisioner, a.Key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = EnsureDatabase(ctx, provisioner, a); !errors.Is(err, ErrDatabaseBusy) {
		t.Fatal("parallel ensure was not excluded", err)
	}
	if err = DeleteDatabase(ctx, provisioner, a.Key); !errors.Is(err, ErrDatabaseBusy) {
		t.Fatal("parallel deletion was not excluded", err)
	}
	_ = locked.conn.Close(ctx)
	if err = DeleteDatabase(ctx, provisioner, a.Key); err != nil {
		t.Fatal("delete with active application session", err)
	}
	if _, err = EnsureDatabase(ctx, provisioner, a); !errors.Is(err, ErrDatabaseDeleted) {
		t.Fatal("late ensure recreated deleted state", err)
	}
	if err = DeleteDatabase(ctx, provisioner, a.Key); err != nil {
		t.Fatal("repeated deletion", err)
	}
	if err = other.QueryRow(ctx, "SELECT value FROM public.marker").Scan(&count); err != nil || count != 99 {
		t.Fatal("deletion changed the other database", err)
	}

	collision := spec("unrecorded")
	names, _ := DatabaseNames(collision.Key)
	exec(bootstrap, "CREATE ROLE "+quoted(names.Owner)+" NOLOGIN")
	if _, err = EnsureDatabase(ctx, provisioner, collision); !errors.Is(err, ErrDatabaseOwnership) {
		t.Fatal("unrecorded role was adopted", err)
	}
	if err = DeleteDatabase(ctx, provisioner, collision.Key); !errors.Is(err, ErrDatabaseOwnership) {
		t.Fatal("unrecorded role was deleted", err)
	}
	exec(bootstrap, "DROP ROLE "+quoted(names.Owner))
	if err = DeleteDatabase(ctx, provisioner, collision.Key); err != nil {
		t.Fatal(err)
	}
	if _, err = EnsureDatabase(ctx, provisioner, collision); !errors.Is(err, ErrDatabaseDeleted) {
		t.Fatal("delete-before-create allowed late creation", err)
	}
	if err = DeleteDatabase(ctx, provisioner, b.Key); err != nil {
		t.Fatal(err)
	}
}

func TestDatabaseClientDoesNotUseGlobalProviders(t *testing.T) {
	previousTrace, previousMeter, previousLog := otel.GetTracerProvider(), otel.GetMeterProvider(), slog.Default()
	exporter := tracetest.NewInMemoryExporter()
	traces := traceSDK.NewTracerProvider(traceSDK.WithSyncer(exporter))
	reader := metricSDK.NewManualReader()
	meters := metricSDK.NewMeterProvider(metricSDK.WithReader(reader))
	var logs bytes.Buffer
	otel.SetTracerProvider(traces)
	otel.SetMeterProvider(meters)
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer func() {
		otel.SetTracerProvider(previousTrace)
		otel.SetMeterProvider(previousMeter)
		slog.SetDefault(previousLog)
		_ = traces.Shutdown(context.Background())
		_ = meters.Shutdown(context.Background())
	}()
	ctx, parent := traces.Tracer("test").Start(context.Background(), "caller")
	_, err := EnsureDatabase(ctx, Options{Password: "private-administrator"}, DatabaseSpec{Key: DatabaseKey{"private-scope", "private-resource"}, Password: strings.Repeat("a", 64)})
	if err == nil {
		t.Fatal("invalid specification accepted")
	}
	if _, err := PrepareDatabaseCredentials(ctx, Options{Password: "private-administrator"}, DatabaseKey{"private-scope", "private-resource"}); err == nil {
		t.Fatal("invalid preparation accepted")
	}
	session := &provisionSession{ctx: ctx, key: DatabaseKey{"private-scope", "private-resource"}, names: DatabaseIdentity{"private-database", "private-owner", "private-login"}}
	if err := session.quarantine(databaseRecord{}, 0); !errors.Is(err, ErrDatabaseIsolation) {
		t.Fatal("invalid quarantine bound accepted", err)
	}
	if len(exporter.GetSpans()) != 0 {
		t.Fatal("an unbound client used the global tracer")
	}
	parent.End()
	var measured metricdata.ResourceMetrics
	if err = reader.Collect(context.Background(), &measured); err != nil {
		t.Fatal(err)
	}
	if len(measured.ScopeMetrics) != 0 || logs.Len() != 0 {
		t.Fatal("an unbound client used global metrics or logging")
	}
}

func TestSchemaOperationValidation(t *testing.T) {
	callback := func(context.Context, *sql.Conn, DatabaseIdentity) error {
		t.Fatal("invalid operation called schema code")
		return nil
	}
	password, _ := NewDatabasePassword()
	spec := DatabaseSpec{Key: DatabaseKey{"installation", "browser"}, Password: password, ConnectionLimit: 8, ManagedSchema: true}
	if err := WithDatabaseOwner(nil, Options{}, spec, callback); err == nil {
		t.Fatal("nil context accepted")
	}
	if err := WithDatabaseOwner(context.Background(), Options{}, spec, nil); err == nil {
		t.Fatal("nil callback accepted")
	}
	if err := WithDatabaseOwner(context.Background(), Options{}, spec, callback); !errors.Is(err, ErrDatabaseServer) {
		t.Fatal("unpinned server accepted", err)
	}
	spec.ManagedSchema = false
	if err := WithDatabaseOwner(context.Background(), Options{}, spec, callback); err == nil {
		t.Fatal("unmanaged database accepted")
	}
	spec.ManagedSchema = true
	spec.Password = "invalid"
	if err := WithDatabaseOwner(context.Background(), Options{}, spec, callback); err == nil {
		t.Fatal("invalid credential accepted")
	}
}
