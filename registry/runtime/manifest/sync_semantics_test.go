package manifest

// The semantics of the sync function on PostgreSQL, from the independent verification of PR #270
// (ST-SUP-RP15-LC02-T05 and T06): the copy direction of the writer marker, the backfill bypass, the fill value of a
// value that does not convert, and the refusal of a hostile name or constant.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// that owner; a runtime role holds SELECT, INSERT and UPDATE. It gives the superuser pool and the names of the owner
// and the runtime role.
func formSync(t *testing.T, kType, uType, kDefault, conversion, fill string, hasFill bool) (*sql.DB, string, string) {
	t.Helper()
	admin, _ := database(t)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	owner, runtime := "form_owner_"+suffix, "form_runtime_"+suffix
	t.Cleanup(func() {
		_, _ = admin.Exec("DROP SCHEMA IF EXISTS steps CASCADE; DROP TABLE IF EXISTS public.v; DROP OWNED BY " + runtime + "; DROP OWNED BY " + owner +
			"; DROP ROLE IF EXISTS " + runtime + "; DROP ROLE IF EXISTS " + owner)
	})
	c, ok := SyncConversion(conversion)
	if !ok {
		t.Fatalf("no conversion %s", conversion)
	}
	function, err := SyncFunction("steps", "v_sync", 1, Sync{Change: "v-k", Owner: owner, K: "k", U: "u", Conversion: c, Fill: fill, HasFill: hasFill})
	if err != nil {
		t.Fatal(err)
	}
	def := ""
	if kDefault != "" {
		def = " DEFAULT " + kDefault
	}
	mustExec(t, admin, fmt.Sprintf(`CREATE ROLE %[1]s NOLOGIN NOSUPERUSER NOBYPASSRLS; CREATE ROLE %[2]s NOLOGIN NOSUPERUSER NOBYPASSRLS;
GRANT %[2]s TO %[1]s;
CREATE SCHEMA steps AUTHORIZATION %[1]s;
CREATE TABLE public.v (tenant text NOT NULL DEFAULT current_setting('app.tenant'), id integer NOT NULL, k %[3]s%[4]s, u %[5]s, PRIMARY KEY (tenant, id));
ALTER TABLE public.v OWNER TO %[1]s;
ALTER TABLE public.v ENABLE ROW LEVEL SECURITY; ALTER TABLE public.v FORCE ROW LEVEL SECURITY;
CREATE POLICY t ON public.v USING (tenant = current_setting('app.tenant', true)) WITH CHECK (tenant = current_setting('app.tenant', true));
GRANT SELECT, INSERT, UPDATE ON public.v TO %[2]s; GRANT USAGE ON SCHEMA steps TO %[2]s;
SET ROLE %[1]s;
%[6]s;
CREATE TRIGGER v_sync BEFORE INSERT OR UPDATE ON public.v FOR EACH ROW EXECUTE FUNCTION steps.v_sync();
RESET ROLE;`, owner, runtime, kType, def, uType, function))
	return admin, owner, runtime
}

// formRow runs prelude and statement in one transaction of one connection, then reads k and u of the row id as text.
func formRow(t *testing.T, admin *sql.DB, prelude, statement string, id int) (k, u sql.NullString, err error) {
	t.Helper()
	ctx := context.Background()
	conn, cerr := admin.Conn(ctx)
	if cerr != nil {
		t.Fatal(cerr)
	}
	defer conn.Close()
	tx, cerr := conn.BeginTx(ctx, nil)
	if cerr != nil {
		t.Fatal(cerr)
	}
	defer tx.Rollback()
	if _, err = tx.Exec(prelude + "; SET LOCAL app.tenant = 't'"); err != nil {
		return
	}
	if _, err = tx.Exec(statement); err != nil {
		return
	}
	if cerr := tx.QueryRow("SELECT k::text, u::text FROM public.v WHERE id = $1", id).Scan(&k, &u); cerr != nil {
		err = cerr
		return
	}
	err = tx.Commit()
	return
}

func TestSyncSemantics(t *testing.T) {
	ns := func(s string) sql.NullString { return sql.NullString{String: s, Valid: true} }
	null := sql.NullString{}

	t.Run("ST-SUP-RP15-LC02-T06 the writer marker gives the copy direction of each release, with a declared default", func(t *testing.T) {
		admin, _, runtime := formSync(t, "integer", "text", "5", "int4-text", "", false)
		before := "SET LOCAL ROLE " + runtime
		step := before + "; SET LOCAL stego.schema_step = 'v-k'"
		other := before + "; SET LOCAL stego.schema_step = 'v-k-2,x-v-k,v'"
		cases := []struct {
			name, prelude, statement string
			id                       int
			k, u                     sql.NullString
		}{
			{"before: INSERT of K", before, "INSERT INTO public.v (id, k) VALUES (1, 7)", 1, ns("7"), ns("7")},
			{"before: INSERT with the default of K", before, "INSERT INTO public.v (id) VALUES (2)", 2, ns("5"), ns("5")},
			{"before: INSERT of NULL K", before, "INSERT INTO public.v (id, k) VALUES (3, NULL)", 3, null, null},
			{"step: INSERT of U", step, "INSERT INTO public.v (id, u) VALUES (4, '8')", 4, ns("8"), ns("8")},
			{"step: INSERT with no U takes NULL over the default of K", step, "INSERT INTO public.v (id) VALUES (5)", 5, null, null},
			{"another change in the marker: INSERT of K", other, "INSERT INTO public.v (id, k) VALUES (6, 9)", 6, ns("9"), ns("9")},
			{"before: UPDATE of K", before, "UPDATE public.v SET k = 11 WHERE id = 1", 1, ns("11"), ns("11")},
			{"step: UPDATE of U", step, "UPDATE public.v SET u = '12' WHERE id = 1", 1, ns("12"), ns("12")},
			{"before: UPDATE of U is taken back from K", before, "UPDATE public.v SET u = '99' WHERE id = 1", 1, ns("12"), ns("12")},
			{"step: UPDATE of K is taken back from U", step, "UPDATE public.v SET k = 98 WHERE id = 1", 1, ns("12"), ns("12")},
		}
		for _, c := range cases {
			k, u, err := formRow(t, admin, c.prelude, c.statement, c.id)
			if err != nil || k != c.k || u != c.u {
				t.Errorf("%s: k %v u %v (error %v), want k %v u %v", c.name, k, u, err, c.k, c.u)
			}
		}
		for _, c := range []struct{ name, prelude, statement string }{
			{"before: INSERT of K and another U", before, "INSERT INTO public.v (id, k, u) VALUES (20, 1, '2')"},
			{"step: UPDATE of both to values that differ", step, "UPDATE public.v SET k = 1, u = '2' WHERE id = 1"},
			{"before: UPDATE of both to values that differ", before, "UPDATE public.v SET k = 1, u = '2' WHERE id = 1"},
		} {
			_, _, err := formRow(t, admin, c.prelude, c.statement, 1)
			var pg *pgconn.PgError
			if !errors.As(err, &pg) || pg.Code != "23514" {
				t.Errorf("%s gives %v, want 23514", c.name, err)
			}
		}
	})

	t.Run("ST-SUP-RP15-LC02-T06 the backfill bypass holds for the owner role only", func(t *testing.T) {
		admin, owner, runtime := formSync(t, "text", "text", "", "identity", "", false)
		if _, _, err := formRow(t, admin, "SET LOCAL ROLE "+runtime, "INSERT INTO public.v (id, k) VALUES (1, 'a')", 1); err != nil {
			t.Fatal(err)
		}
		backfill := "; SET LOCAL stego.backfill_change = 'v-k'"
		cases := []struct {
			name, prelude string
			bypass        bool
		}{
			{"the owner", "SET LOCAL ROLE " + owner + backfill, true},
			{"the owner, then SET ROLE to the runtime role", "SET LOCAL ROLE " + owner + "; SET LOCAL ROLE " + runtime + backfill, false},
			{"the runtime role", "SET LOCAL ROLE " + runtime + backfill, false},
			{"the superuser (not the owner)", "SELECT 1" + backfill, false},
			{"the owner with another change", "SET LOCAL ROLE " + owner + "; SET LOCAL stego.backfill_change = 'v-k-2'", false},
			{"the owner with a list that holds the change", "SET LOCAL ROLE " + owner + "; SET LOCAL stego.backfill_change = 'v-k,x'", false},
			{"the runtime role with the session setting", "SET LOCAL ROLE " + runtime + "; SET stego.backfill_change = 'v-k'", false},
		}
		for i, c := range cases {
			want := fmt.Sprintf("b%d", i)
			k, u, err := formRow(t, admin, c.prelude, "UPDATE public.v SET u = '"+want+"' WHERE id = 1", 1)
			if err != nil {
				t.Errorf("%s: %v", c.name, err)
				continue
			}
			bypassed := u.String == want && k.String != want
			if bypassed != c.bypass {
				t.Errorf("%s: k %v u %v, bypass %v, want bypass %v", c.name, k, u, bypassed, c.bypass)
			}
			// Put K and U equal again as the release before.
			mustExec(t, admin, "SET app.tenant = 't'; UPDATE public.v SET k = 'a' WHERE id = 1; RESET app.tenant; RESET stego.backfill_change")
		}
	})

	t.Run("ST-SUP-RP15-LC02-T06 a value that does not convert gives the fill value and never an error", func(t *testing.T) {
		for _, c := range []struct {
			conversion, kType, uType, fill string
			hasFill                        bool
			values                         map[string]sql.NullString // k literal -> u text
		}{
			{"text-int4", "text", "integer", "0", true, map[string]sql.NullString{"'12'": ns("12"), "' 13 '": ns("13"), "'abc'": ns("0"), "'99999999999'": ns("0"), "''": ns("0"), "'1.5'": ns("0"), "NULL": null}},
			{"text-int4", "text", "integer", "", false, map[string]sql.NullString{"'abc'": null, "'7'": ns("7")}},
			{"text-int8", "text", "bigint", "-1", true, map[string]sql.NullString{"'99999999999999999999'": ns("-1"), "'5'": ns("5")}},
			{"text-numeric", "text", "numeric", "0", true, map[string]sql.NullString{"'1e9999999'": ns("0"), "'x'": ns("0"), "'NaN'": ns("NaN"), "'2.5'": ns("2.5")}},
			{"text-bool", "text", "boolean", "false", true, map[string]sql.NullString{"'maybe'": ns("false"), "'yes'": ns("true")}},
			{"text-uuid", "text", "uuid", "00000000-0000-0000-0000-000000000000", true, map[string]sql.NullString{"'zz'": ns("00000000-0000-0000-0000-000000000000")}},
			{"bigint-integer", "bigint", "integer", "-1", true, map[string]sql.NullString{"3000000000": ns("-1"), "-2147483649": ns("-1"), "2147483647": ns("2147483647"), "-2147483648": ns("-2147483648")}},
		} {
			t.Run(c.conversion+" fill "+c.fill, func(t *testing.T) {
				admin, _, runtime := formSync(t, c.kType, c.uType, "", c.conversion, c.fill, c.hasFill)
				id := 0
				for k, want := range c.values {
					id++
					// INSERT and UPDATE of the release before, each as the runtime role.
					_, u, err := formRow(t, admin, "SET LOCAL ROLE "+runtime, fmt.Sprintf("INSERT INTO public.v (id, k) VALUES (%d, %s)", id, k), id)
					if err != nil || u != want {
						t.Errorf("INSERT k %s: u %v (error %v), want %v", k, u, err, want)
					}
					_, u, err = formRow(t, admin, "SET LOCAL ROLE "+runtime, fmt.Sprintf("UPDATE public.v SET k = %s WHERE id = %d", k, id), id)
					if err != nil || u != want {
						t.Errorf("UPDATE k %s: u %v (error %v), want %v", k, u, err, want)
					}
				}
			})
		}
		// The back direction (the release of the step writes U) is a cast with no guard, which ST-DECL-INV63 makes
		// total. A value that does not convert back fails closed: the write is refused with 22P02 and no row is
		// written.
		t.Run("a write of U that does not convert back fails closed", func(t *testing.T) {
			admin, _, runtime := formSync(t, "integer", "text", "", "int4-text", "", false)
			_, _, err := formRow(t, admin, "SET LOCAL ROLE "+runtime+"; SET LOCAL stego.schema_step = 'v-k'", "INSERT INTO public.v (id, u) VALUES (1, 'abc')", 1)
			var pg *pgconn.PgError
			if !errors.As(err, &pg) || pg.Code != "22P02" {
				t.Errorf("the release of the step writes u = 'abc' under int4-text: %v, want 22P02", err)
			}
			var rows int
			if err := admin.QueryRow("SET app.tenant = 't'; SELECT count(*) FROM public.v").Scan(&rows); err == nil && rows != 0 {
				t.Errorf("the refused write left %d rows", rows)
			}
		})
	})

	t.Run("ST-SUP-RP15-LC02-T05 SyncSource and SyncFunction refuse a dollar sign, a backslash and a control character in every input", func(t *testing.T) {
		c, _ := SyncConversion("text-int4")
		ok := Sync{Change: "v-k", Owner: "owner", K: "k", U: "u", Conversion: c, Fill: "0", HasFill: true}
		hostile := []string{"$", "$stego_sync$", "a$b", `\`, `a\b`, "\x00", "\x01", "\n", "\t", "\x1b", "\x7f", "\u0085", " "}
		fields := []struct {
			name string
			set  func(s *Sync, schema, function *string, v string)
		}{
			{"K", func(s *Sync, _, _ *string, v string) { s.K = "k" + v }},
			{"U", func(s *Sync, _, _ *string, v string) { s.U = "u" + v }},
			{"Change", func(s *Sync, _, _ *string, v string) { s.Change = "v-k" + v }},
			{"Owner", func(s *Sync, _, _ *string, v string) { s.Owner = "owner" + v }},
			{"Fill", func(s *Sync, _, _ *string, v string) { s.Fill = "0" + v }},
			{"schema", func(_ *Sync, schema, _ *string, v string) { *schema = "steps" + v }},
			{"function", func(_ *Sync, _, function *string, v string) { *function = "f" + v }},
		}
		admin, _ := database(t)
		mustExec(t, admin, "CREATE SCHEMA steps; CREATE TABLE public.canary (n int); CREATE TABLE public.v (k text, u integer)")
		for _, f := range fields {
			for _, h := range hostile {
				s, schema, function := ok, "steps", "f"
				f.set(&s, &schema, &function, h)
				statement, err := SyncFunction(schema, function, 1, s)
				if err != nil {
					continue
				}
				// Accepted: the statement must make one function and run nothing else.
				_, execErr := admin.Exec("SET check_function_bodies = off; " + statement + "; RESET check_function_bodies")
				var leaked int
				_ = admin.QueryRow("SELECT count(*) FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname LIKE 'steps%'").Scan(&leaked)
				t.Errorf("%s with %q is accepted (statement runs: %v; functions now %d)", f.name, h, execErr, leaked)
				_, _ = admin.Exec("DROP SCHEMA IF EXISTS steps CASCADE; CREATE SCHEMA steps")
			}
		}
		// Injection probes: a fill or a name that tries to end the literal, the identifier or the dollar quote.
		for _, probe := range []struct {
			name string
			set  func(s *Sync)
		}{
			{"fill with a quote", func(s *Sync) { s.Fill = "0'; CREATE TABLE public.pwn (x int); --" }},
			{"K with a double quote", func(s *Sync) { s.K = `k"; CREATE TABLE public.pwn (x int); --` }},
			{"U with a double quote", func(s *Sync) { s.U = `u" := 1; CREATE TABLE public.pwn (x int); --` }},
		} {
			s := ok
			probe.set(&s)
			statement, err := SyncFunction("steps", "probe", 1, s)
			if err != nil {
				continue
			}
			_, _ = admin.Exec("SET check_function_bodies = off; " + statement + "; RESET check_function_bodies")
			var pwn bool
			_ = admin.QueryRow("SELECT to_regclass('public.pwn') IS NOT NULL").Scan(&pwn)
			if pwn {
				t.Errorf("%s ran SQL outside the function", probe.name)
			}
		}
	})
}
