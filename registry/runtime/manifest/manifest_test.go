package manifest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/jsell-rh/stego/registry/runtime/internal/pgtest"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

// schema makes the tables of the test: a table with a tenant column, row-level security, a check, a foreign key,
// a unique upsert key with a predicate and a trigger, as the generated migrations and the audit DDL make them.
const schema = `
CREATE TABLE public.folders (
  tenant_id text NOT NULL,
  id uuid NOT NULL,
  CONSTRAINT folders_pkey PRIMARY KEY (tenant_id, id)
);
CREATE TABLE public.books (
  tenant_id text NOT NULL DEFAULT current_setting('app.tenant'),
  id uuid NOT NULL,
  folder_id uuid NOT NULL,
  title character varying(255) NOT NULL,
  note text,
  pages integer,
  CONSTRAINT books_pkey PRIMARY KEY (tenant_id, id),
  CONSTRAINT books_title_check CHECK (char_length(title) >= 1),
  CONSTRAINT books_folder_id_fkey FOREIGN KEY (tenant_id, folder_id) REFERENCES public.folders (tenant_id, id)
);
CREATE UNIQUE INDEX books_folder_id_title_key ON public.books (tenant_id, folder_id, title) WHERE note IS NULL;
ALTER TABLE public.books ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.books FORCE ROW LEVEL SECURITY;
CREATE POLICY stego_tenant ON public.books USING ("tenant_id" = pg_catalog.current_setting('app.tenant', true))
  WITH CHECK ("tenant_id" = pg_catalog.current_setting('app.tenant', true));
CREATE FUNCTION public.books_reject() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'no'; END $$;
CREATE TRIGGER books_reject_row BEFORE UPDATE OR DELETE ON public.books FOR EACH ROW EXECUTE FUNCTION public.books_reject();
CREATE ROLE manifest_reader_` + "%s" + ` LOGIN PASSWORD 'manifest-reader' NOSUPERUSER NOBYPASSRLS;
`

const tenantWall = "(tenant_id = current_setting('app.tenant'::text, true))"

// want is the manifest of the schema.
// testOwner is the role that owns the tables of the test database: the superuser of the server.
var testOwner string

func want() Manifest {
	return Manifest{Owner: testOwner, Tables: []Table{
		{Schema: "public", Name: "folders", Columns: []Column{{Name: "tenant_id", Type: "text", NotNull: true}, {Name: "id", Type: "uuid", NotNull: true}},
			Constraints: []Constraint{{Name: "folders_pkey", Kind: KindPrimaryKey, Definition: "PRIMARY KEY (tenant_id, id)"}},
			Indexes:     []Index{{Name: "folders_pkey", Columns: []string{"tenant_id", "id"}}}},
		{Schema: "public", Name: "books",
			Columns: []Column{{Name: "tenant_id", Type: "text", NotNull: true}, {Name: "id", Type: "uuid", NotNull: true},
				{Name: "folder_id", Type: "uuid", NotNull: true}, {Name: "title", Type: "character varying(255)", NotNull: true},
				{Name: "note", Type: "text"}, {Name: "pages", Type: "integer"}},
			Constraints: []Constraint{{Name: "books_pkey", Kind: KindPrimaryKey, Definition: "PRIMARY KEY (tenant_id, id)"},
				{Name: "books_title_check", Kind: KindCheck, Definition: "CHECK ((char_length((title)::text) >= 1))"},
				{Name: "books_folder_id_fkey", Kind: KindForeignKey,
					Definition: "FOREIGN KEY (tenant_id, folder_id) REFERENCES public.folders(tenant_id, id)"}},
			Indexes: []Index{{Name: "books_pkey", Columns: []string{"tenant_id", "id"}},
				{Name: "books_folder_id_title_key", Columns: []string{"tenant_id", "folder_id", "title"}, Predicate: "(note IS NULL)"}},
			RowSecurity: true,
			Policies:    []Policy{{Name: "stego_tenant", Command: "*", Using: tenantWall, WithCheck: tenantWall}},
			Triggers: []Trigger{{Name: "books_reject_row", Timing: "BEFORE", Row: true, Events: []string{"UPDATE", "DELETE"},
				Function: Function{Name: "public.books_reject", Language: "plpgsql", Source: " BEGIN RAISE EXCEPTION 'no'; END "}}}},
	}}
}

// database makes a database with the schema and gives the superuser pool and a pool of a role that holds no
// privilege on the tables, as the runtime role holds none on the catalog.
func database(t *testing.T) (*sql.DB, *sql.DB) {
	t.Helper()
	dsn := pgtest.Database(t, "manifest")
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	role := "manifest_reader_" + strings.TrimPrefix(u.Path, "/manifest_")
	if err := admin.QueryRow("SELECT current_user").Scan(&testOwner); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(strings.Replace(schema, "manifest_reader_%s", role, 1)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec("DROP OWNED BY " + role + "; DROP ROLE IF EXISTS " + role) })
	u.User = url.UserPassword(role, "manifest-reader")
	reader, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	return admin, reader
}

func TestCheck(t *testing.T) {
	t.Run("ST-SUP-RP15-LC01-T01 Check refuses a database that does not hold the manifest and names each object", func(t *testing.T) {
		ctx := context.Background()
		// Each change makes the database differ from the manifest once, and the fault names the object.
		changes := []struct{ name, statement, fault string }{
			{"a missing table", "DROP TABLE public.books", "table public.books is missing"},
			{"a missing column", "ALTER TABLE public.books DROP COLUMN pages", "table public.books: column pages is missing"},
			{"a changed column type", "ALTER TABLE public.books ALTER COLUMN title TYPE character varying(300)",
				"table public.books: column title has the type character varying(300), and the manifest needs character varying(255)"},
			{"a dropped NOT NULL", "ALTER TABLE public.books ALTER COLUMN folder_id DROP NOT NULL",
				"table public.books: column folder_id is nullable, and the manifest needs NOT NULL"},
			{"a dropped check", "ALTER TABLE public.books DROP CONSTRAINT books_title_check",
				"table public.books: constraint books_title_check is missing"},
			{"a foreign key that is not validated", "ALTER TABLE public.books DROP CONSTRAINT books_folder_id_fkey; " +
				"ALTER TABLE public.books ADD CONSTRAINT books_folder_id_fkey FOREIGN KEY (tenant_id, folder_id) REFERENCES public.folders (tenant_id, id) NOT VALID",
				"table public.books: constraint books_folder_id_fkey is not validated"},
			{"a dropped upsert key index", "DROP INDEX public.books_folder_id_title_key",
				"table public.books: index books_folder_id_title_key is missing"},
			{"an upsert key index on other columns", "DROP INDEX public.books_folder_id_title_key; " +
				"CREATE UNIQUE INDEX books_folder_id_title_key ON public.books (tenant_id, title) WHERE note IS NULL",
				"table public.books: index books_folder_id_title_key does not have the columns (tenant_id, folder_id, title) of the manifest"},
			{"an upsert key index with another predicate", "DROP INDEX public.books_folder_id_title_key; " +
				"CREATE UNIQUE INDEX books_folder_id_title_key ON public.books (tenant_id, folder_id, title)",
				"table public.books: index books_folder_id_title_key does not have the predicate of the manifest"},
			{"an index that is not unique", "DROP INDEX public.books_folder_id_title_key; " +
				"CREATE INDEX books_folder_id_title_key ON public.books (tenant_id, folder_id, title) WHERE note IS NULL",
				"table public.books: index books_folder_id_title_key is not unique"},
			{"an index on an expression", "DROP INDEX public.books_folder_id_title_key; " +
				"CREATE UNIQUE INDEX books_folder_id_title_key ON public.books (tenant_id, folder_id, lower(title)) WHERE note IS NULL",
				"table public.books: index books_folder_id_title_key does not have the columns (tenant_id, folder_id, title) of the manifest"},
			{"disabled row-level security", "ALTER TABLE public.books DISABLE ROW LEVEL SECURITY",
				"table public.books: row-level security is not enabled and forced"},
			{"row-level security that is not forced", "ALTER TABLE public.books NO FORCE ROW LEVEL SECURITY",
				"table public.books: row-level security is not enabled and forced"},
			{"a dropped policy", "DROP POLICY stego_tenant ON public.books", "table public.books: policy stego_tenant is missing"},
			{"a policy with another expression", "ALTER POLICY stego_tenant ON public.books USING (true)",
				"table public.books: policy stego_tenant is not the policy of the manifest"},
			{"a policy of every role that applies to one role", "DO $$ BEGIN CREATE ROLE manifest_one_role NOLOGIN; EXCEPTION WHEN duplicate_object THEN NULL; END $$; ALTER POLICY stego_tenant ON public.books TO manifest_one_role",
				"table public.books: policy stego_tenant is not the policy of the manifest"},
			{"an added permissive policy", "CREATE POLICY open_door ON public.books USING (true)",
				"table public.books: policy open_door is a permissive policy that the manifest does not name"},
			{"a dropped trigger", "DROP TRIGGER books_reject_row ON public.books", "table public.books: trigger books_reject_row is missing"},
			{"a disabled trigger", "ALTER TABLE public.books DISABLE TRIGGER books_reject_row",
				"table public.books: trigger books_reject_row is disabled"},
			{"an added trigger on a table with row-level security", "CREATE TRIGGER books_more AFTER INSERT ON public.books FOR EACH ROW EXECUTE FUNCTION public.books_reject()",
				"table public.books: trigger books_more is a trigger that the manifest does not name"},
			{"a trigger on other events", "DROP TRIGGER books_reject_row ON public.books; " +
				"CREATE TRIGGER books_reject_row BEFORE DELETE ON public.books FOR EACH ROW EXECUTE FUNCTION public.books_reject()",
				"table public.books: trigger books_reject_row is not the trigger of the manifest"},
		}
		for _, change := range changes {
			t.Run(change.name, func(t *testing.T) {
				admin, reader := database(t)
				if err := Check(ctx, reader, want()); err != nil {
					t.Fatalf("Check refused the database of the manifest: %v", err)
				}
				if _, err := admin.Exec(change.statement); err != nil {
					t.Fatal(err)
				}
				err := Check(ctx, reader, want())
				var mismatch *Mismatch
				if !errors.As(err, &mismatch) || !errors.Is(err, ErrNotSatisfied) {
					t.Fatalf("Check gave %v, want a mismatch", err)
				}
				if len(mismatch.Faults) != 1 || mismatch.Faults[0] != change.fault {
					t.Fatalf("Check gave the faults %q, want %q", mismatch.Faults, change.fault)
				}
			})
		}

		t.Run("a policy of two roles named in the other order is taken", func(t *testing.T) {
			admin, reader := database(t)
			suffix := fmt.Sprintf("%d", time.Now().UnixNano())
			a, b := "manifest_a_"+suffix, "manifest_b_"+suffix
			if _, err := admin.Exec("CREATE ROLE " + a + " NOLOGIN; CREATE ROLE " + b + " NOLOGIN; ALTER POLICY stego_tenant ON public.books TO " + b + ", " + a); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_, _ = admin.Exec("DROP OWNED BY " + a + ", " + b + "; DROP ROLE IF EXISTS " + a + "; DROP ROLE IF EXISTS " + b)
			})
			m := want()
			for i := range m.Tables[1].Policies {
				m.Tables[1].Policies[i].Roles = []string{a, b}
			}
			if err := Check(ctx, reader, m); err != nil {
				t.Fatalf("Check refused a policy whose roles the catalog keeps in the other order: %v", err)
			}
		})

		t.Run("more objects are allowed, and a nullable column may be NOT NULL", func(t *testing.T) {
			admin, reader := database(t)
			for _, statement := range []string{
				"ALTER TABLE public.books ADD COLUMN extra text",
				"CREATE INDEX books_extra ON public.books (extra)",
				"CREATE POLICY narrow ON public.books AS RESTRICTIVE USING (true)",
				"CREATE TABLE public.other (x int)",
				"ALTER TABLE public.books ALTER COLUMN note SET NOT NULL",
			} {
				if _, err := admin.Exec(statement); err != nil {
					t.Fatal(err)
				}
			}
			if err := Check(ctx, reader, want()); err != nil {
				t.Fatalf("Check refused a database with more objects: %v", err)
			}
		})

		t.Run("more objects are allowed on a table without row-level security", func(t *testing.T) {
			admin, reader := database(t)
			for _, statement := range []string{
				"CREATE FUNCTION public.folders_note() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$",
				"CREATE TRIGGER folders_note AFTER INSERT ON public.folders FOR EACH ROW EXECUTE FUNCTION public.folders_note()",
				"CREATE UNIQUE INDEX folders_more ON public.folders (id)",
			} {
				if _, err := admin.Exec(statement); err != nil {
					t.Fatal(err)
				}
			}
			if err := Check(ctx, reader, want()); err != nil {
				t.Fatalf("Check refused a trigger on a table without row-level security: %v", err)
			}
		})

		t.Run("each fault of several is named, and an invalid manifest or database is refused", func(t *testing.T) {
			admin, reader := database(t)
			if _, err := admin.Exec("DROP INDEX public.books_folder_id_title_key; DROP TRIGGER books_reject_row ON public.books"); err != nil {
				t.Fatal(err)
			}
			var mismatch *Mismatch
			if err := Check(ctx, reader, want()); !errors.As(err, &mismatch) || len(mismatch.Faults) != 2 ||
				!strings.Contains(err.Error(), "books_folder_id_title_key") || !strings.Contains(err.Error(), "books_reject_row") {
				t.Fatalf("Check gave %v, want both faults", err)
			}
			if err := Check(ctx, nil, want()); err == nil {
				t.Fatal("Check took no database")
			}
			for _, bad := range []Manifest{
				{Tables: []Table{{Schema: "public"}}},
				{Tables: []Table{{Schema: "public", Name: "a"}, {Schema: "public", Name: "a"}}},
				{Tables: []Table{{Schema: "public", Name: "a", Columns: []Column{{Name: "x"}}}}},
				{Tables: []Table{{Schema: "public", Name: "a", Constraints: []Constraint{{Name: "k", Kind: "x"}}}}},
				{Tables: []Table{{Schema: "public", Name: "a", Constraints: []Constraint{{Name: "k", Kind: KindForeignKey}}}}},
				{Tables: []Table{{Schema: "public", Name: "a", Triggers: []Trigger{{Name: "t", Timing: "BEFORE", Events: []string{"INSERT"}}}}}},
				{Tables: []Table{{Schema: "public", Name: "a", Indexes: []Index{{Name: "i"}}}}},
				{Tables: []Table{{Schema: "public", Name: "a", Policies: []Policy{{Name: "p", Command: "x"}}}}},
				{Tables: []Table{{Schema: "public", Name: "a", Triggers: []Trigger{{Name: "t", Timing: "BEFORE", Function: Function{Name: "f", Language: "sql", Source: "x"}}}}}},
				{Tables: []Table{{Schema: "public", Name: "a", Triggers: []Trigger{{Name: "t", Timing: "SOON", Events: []string{"INSERT"}, Function: Function{Name: "f", Language: "sql", Source: "x"}}}}}},
			} {
				if err := Check(ctx, reader, bad); err == nil || errors.Is(err, ErrNotSatisfied) {
					t.Fatalf("Check took the invalid manifest %+v: %v", bad, err)
				}
			}
			closedDB, err := sql.Open("pgx", "postgres://127.0.0.1:1/none?connect_timeout=1")
			if err != nil {
				t.Fatal(err)
			}
			_ = closedDB.Close()
			if err := Check(ctx, closedDB, want()); err == nil || errors.Is(err, ErrNotSatisfied) {
				t.Fatalf("Check of a closed database gave %v, want an error that is not a mismatch", err)
			}
		})
	})
}

// redTeam are the hostile changes of the security review of PR 218: each keeps every name of the manifest and
// weakens the table, and each needs the owner role. Check must refuse each one and name the object.
var redTeam = []struct{ name, statement, fault string }{
	{"a trigger function with another body",
		`CREATE OR REPLACE FUNCTION public.books_reject() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NULL; END $$`,
		"table public.books: trigger books_reject_row runs the function public.books_reject, which is not the function of the manifest"},
	{"a trigger function with another body that is SECURITY DEFINER",
		`CREATE OR REPLACE FUNCTION public.books_reject() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER AS $$ BEGIN IF TG_OP = 'DELETE' THEN RETURN OLD; END IF; RETURN NEW; END $$`,
		"table public.books: trigger books_reject_row runs the function public.books_reject, which is not the function of the manifest"},
	{"a trigger function that another role owns",
		`CREATE ROLE manifest_other_owner; ALTER FUNCTION public.books_reject() OWNER TO manifest_other_owner`,
		"table public.books: trigger books_reject_row runs the function public.books_reject, which the owner of the table does not own"},
	{"a trigger with WHEN (false)",
		`DROP TRIGGER books_reject_row ON public.books; CREATE TRIGGER books_reject_row BEFORE UPDATE OR DELETE ON public.books FOR EACH ROW WHEN (false) EXECUTE FUNCTION public.books_reject()`,
		"table public.books: trigger books_reject_row is not the trigger of the manifest"},
	{"a trigger on UPDATE OF a column that no write sets",
		`ALTER TABLE public.books ADD COLUMN never text; DROP TRIGGER books_reject_row ON public.books; CREATE TRIGGER books_reject_row BEFORE UPDATE OF never OR DELETE ON public.books FOR EACH ROW EXECUTE FUNCTION public.books_reject()`,
		"table public.books: trigger books_reject_row is not the trigger of the manifest"},
	{"an added BEFORE INSERT trigger that drops each row",
		`CREATE FUNCTION public.drop_row() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NULL; END $$; CREATE TRIGGER a_drop BEFORE INSERT ON public.books FOR EACH ROW EXECUTE FUNCTION public.drop_row()`,
		"table public.books: trigger a_drop is a trigger that the manifest does not name"},
	{"a rule that copies the rows of every tenant out",
		`CREATE TABLE public.leak (LIKE public.books); CREATE RULE books_leak AS ON INSERT TO public.books DO ALSO INSERT INTO public.leak VALUES (NEW.*)`,
		"table public.books: rule books_leak is a rule that the manifest does not allow"},
	{"a check of the same name with CHECK (true)",
		`ALTER TABLE public.books DROP CONSTRAINT books_title_check; ALTER TABLE public.books ADD CONSTRAINT books_title_check CHECK (true)`,
		"table public.books: constraint books_title_check is not the constraint of the manifest"},
	{"a foreign key of the same name and table on other columns with ON DELETE CASCADE",
		`ALTER TABLE public.books DROP CONSTRAINT books_folder_id_fkey; ALTER TABLE public.folders ADD CONSTRAINT folders_id_u UNIQUE (id); ALTER TABLE public.books ADD CONSTRAINT books_folder_id_fkey FOREIGN KEY (folder_id) REFERENCES public.folders (id) ON DELETE CASCADE`,
		"table public.books: constraint books_folder_id_fkey is not the constraint of the manifest"},
	{"a foreign key that is deferrable",
		`ALTER TABLE public.books DROP CONSTRAINT books_folder_id_fkey; ALTER TABLE public.books ADD CONSTRAINT books_folder_id_fkey FOREIGN KEY (tenant_id, folder_id) REFERENCES public.folders (tenant_id, id) DEFERRABLE INITIALLY DEFERRED`,
		"table public.books: constraint books_folder_id_fkey is not the constraint of the manifest"},
	{"a unique index on one column whose name holds the old separator",
		"ALTER TABLE public.books ADD COLUMN \"folder_id\x1ftitle\" text; DROP INDEX public.books_folder_id_title_key; CREATE UNIQUE INDEX books_folder_id_title_key ON public.books (tenant_id, \"folder_id\x1ftitle\") WHERE note IS NULL",
		"table public.books: index books_folder_id_title_key does not have the columns (tenant_id, folder_id, title) of the manifest"},
	{"a tenant column with a case-insensitive collation",
		`CREATE COLLATION public.ci (provider = icu, locale = 'und-u-ks-level2', deterministic = false); DROP POLICY stego_tenant ON public.books; ALTER TABLE public.books DROP CONSTRAINT books_folder_id_fkey; ALTER TABLE public.books ALTER COLUMN tenant_id TYPE text COLLATE public.ci; ALTER TABLE public.books ADD CONSTRAINT books_folder_id_fkey FOREIGN KEY (tenant_id, folder_id) REFERENCES public.folders (tenant_id, id); CREATE POLICY stego_tenant ON public.books USING (tenant_id = pg_catalog.current_setting('app.tenant', true)) WITH CHECK (tenant_id = pg_catalog.current_setting('app.tenant', true))`,
		"table public.books: column tenant_id has the collation public.ci, and the manifest needs the default or the C collation"},
	{"an upsert key index with NULLS NOT DISTINCT",
		`DROP INDEX public.books_folder_id_title_key; CREATE UNIQUE INDEX books_folder_id_title_key ON public.books (tenant_id, folder_id, title) NULLS NOT DISTINCT WHERE note IS NULL`,
		"table public.books: index books_folder_id_title_key treats nulls as not distinct"},
	{"an upsert key index with another operator class",
		`DROP INDEX public.books_folder_id_title_key; CREATE UNIQUE INDEX books_folder_id_title_key ON public.books (tenant_id, folder_id, title varchar_pattern_ops) WHERE note IS NULL`,
		"table public.books: index books_folder_id_title_key has an operator class or a collation that is not the default"},
	{"an upsert key index with another collation",
		`DROP INDEX public.books_folder_id_title_key; CREATE UNIQUE INDEX books_folder_id_title_key ON public.books (tenant_id, folder_id, title COLLATE "POSIX") WHERE note IS NULL`,
		"table public.books: index books_folder_id_title_key has an operator class or a collation that is not the default"},
}

// redTeam2 are the hostile changes of the delta review of PR 218 at 75acc7c9 (N1 to N5).
var redTeam2 = []struct{ name, statement, fault string }{
	{"a partitioned table with a copying trigger on its partition",
		`DROP TABLE public.books;
CREATE TABLE public.books (tenant_id text NOT NULL DEFAULT current_setting('app.tenant'), id uuid NOT NULL, folder_id uuid NOT NULL,
  title character varying(255) NOT NULL, note text, pages integer,
  CONSTRAINT books_pkey PRIMARY KEY (tenant_id, id), CONSTRAINT books_title_check CHECK (char_length(title) >= 1),
  CONSTRAINT books_folder_id_fkey FOREIGN KEY (tenant_id, folder_id) REFERENCES public.folders (tenant_id, id)) PARTITION BY LIST (tenant_id);
CREATE UNIQUE INDEX books_folder_id_title_key ON public.books (tenant_id, folder_id, title) WHERE note IS NULL;
ALTER TABLE public.books ENABLE ROW LEVEL SECURITY; ALTER TABLE public.books FORCE ROW LEVEL SECURITY;
CREATE POLICY stego_tenant ON public.books USING ("tenant_id" = pg_catalog.current_setting('app.tenant', true)) WITH CHECK ("tenant_id" = pg_catalog.current_setting('app.tenant', true));
CREATE TRIGGER books_reject_row BEFORE UPDATE OR DELETE ON public.books FOR EACH ROW EXECUTE FUNCTION public.books_reject();
CREATE TABLE public.books_all PARTITION OF public.books DEFAULT;
CREATE TABLE public.leak (LIKE public.books);
CREATE FUNCTION public.copy_row() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN INSERT INTO public.leak SELECT NEW.*; RETURN NEW; END $$;
CREATE TRIGGER copy BEFORE INSERT ON public.books_all FOR EACH ROW EXECUTE FUNCTION public.copy_row();`,
		"table public.books: the table is partitioned, and the manifest needs a plain table"},
	{"a table made a child of an open parent by INHERIT",
		`CREATE TABLE public.open_parent (LIKE public.books); ALTER TABLE public.books INHERIT public.open_parent`,
		"table public.books: the table is a partition or a child of another table"},
	{"a table that another table inherits from",
		`CREATE TABLE public.open_child () INHERITS (public.folders)`,
		"table public.folders: another table inherits from the table"},
	{"an extra foreign key with ON DELETE CASCADE to an open table",
		`CREATE TABLE public.evil (id uuid PRIMARY KEY); ALTER TABLE public.books ADD CONSTRAINT books_evil_fkey FOREIGN KEY (folder_id) REFERENCES public.evil (id) ON DELETE CASCADE`,
		"table public.books: constraint books_evil_fkey is a foreign key that the manifest does not name"},
	{"an extra foreign key with no action to an open table",
		`CREATE TABLE public.evil (id uuid PRIMARY KEY); ALTER TABLE public.books ADD CONSTRAINT books_evil_fkey FOREIGN KEY (folder_id) REFERENCES public.evil (id)`,
		"table public.books: constraint books_evil_fkey is a foreign key that the manifest does not name"},
	{"a foreign key of another table to a manifest table with ON DELETE SET NULL",
		`CREATE TABLE public.notes (tenant_id text, folder_id uuid, FOREIGN KEY (tenant_id, folder_id) REFERENCES public.folders (tenant_id, id) ON DELETE SET NULL)`,
		"table public.folders: the foreign key public.notes.notes_tenant_id_folder_id_fkey refers to the table with an action that writes"},
	{"the table and the function moved to a third role",
		`CREATE ROLE manifest_evil_owner NOLOGIN; ALTER TABLE public.books OWNER TO manifest_evil_owner; ALTER FUNCTION public.books_reject() OWNER TO manifest_evil_owner`,
		"table public.books: the table belongs to the role manifest_evil_owner, and the manifest needs %owner%"},
	{"a reader role whose sessions skip triggers",
		`ALTER ROLE %reader% SET session_replication_role = replica`,
		"the session runs with session_replication_role replica, which skips the triggers"},
	{"control: a function setting changed",
		`ALTER FUNCTION public.books_reject() SET search_path = public`,
		"table public.books: trigger books_reject_row runs the function public.books_reject, which is not the function of the manifest"},
	{"control: a constraint trigger added",
		`CREATE CONSTRAINT TRIGGER zz AFTER INSERT ON public.books FOR EACH ROW EXECUTE FUNCTION public.books_reject()`,
		"table public.books: trigger zz is a trigger that the manifest does not name"},
}

func TestRedTeam2(t *testing.T) {
	t.Run("ST-SUP-RP15-LC01-T03 Check refuses a table that is partitioned, inherits, has another owner or foreign key, and a session that skips triggers", func(t *testing.T) {
		ctx := context.Background()
		for _, change := range redTeam2 {
			t.Run(change.name, func(t *testing.T) {
				admin, reader := database(t)
				t.Cleanup(func() {
					_, _ = admin.Exec("DROP OWNED BY manifest_evil_owner; DROP ROLE IF EXISTS manifest_evil_owner")
				})
				if err := Check(ctx, reader, want()); err != nil {
					t.Fatalf("Check refused the database of the manifest: %v", err)
				}
				var role string
				if err := reader.QueryRow("SELECT current_user").Scan(&role); err != nil {
					t.Fatal(err)
				}
				if _, err := admin.Exec(strings.ReplaceAll(change.statement, "%reader%", role)); err != nil {
					t.Fatal(err)
				}
				// A role setting takes effect in a new session.
				reader.SetMaxIdleConns(0)
				err := Check(ctx, reader, want())
				fault := strings.ReplaceAll(change.fault, "%owner%", testOwner)
				var mismatch *Mismatch
				if !errors.As(err, &mismatch) || !slices.Contains(mismatch.Faults, fault) {
					t.Fatalf("Check gave %v, want the fault %q", err, fault)
				}
			})
		}
	})
}

func TestRedTeam(t *testing.T) {
	t.Run("ST-SUP-RP15-LC01-T02 Check refuses a hostile change that keeps the names of the manifest", func(t *testing.T) {
		ctx := context.Background()
		for _, change := range redTeam {
			t.Run(change.name, func(t *testing.T) {
				admin, reader := database(t)
				t.Cleanup(func() { _, _ = admin.Exec("DROP ROLE IF EXISTS manifest_other_owner") })
				if err := Check(ctx, reader, want()); err != nil {
					t.Fatalf("Check refused the database of the manifest: %v", err)
				}
				if _, err := admin.Exec(change.statement); err != nil {
					t.Fatal(err)
				}
				err := Check(ctx, reader, want())
				var mismatch *Mismatch
				if !errors.As(err, &mismatch) || !slices.Contains(mismatch.Faults, change.fault) {
					t.Fatalf("Check gave %v, want the fault %q", err, change.fault)
				}
			})
		}
	})
}

// partitionedBooks makes books a table partitioned by tenant with one default partition, as the manifest states it
// when the table is marked partitioned.
const partitionedBooks = `DROP TABLE public.books;
CREATE TABLE public.books (tenant_id text NOT NULL DEFAULT current_setting('app.tenant'), id uuid NOT NULL, folder_id uuid NOT NULL,
  title character varying(255) NOT NULL, note text, pages integer,
  CONSTRAINT books_pkey PRIMARY KEY (tenant_id, id), CONSTRAINT books_title_check CHECK (char_length(title) >= 1),
  CONSTRAINT books_folder_id_fkey FOREIGN KEY (tenant_id, folder_id) REFERENCES public.folders (tenant_id, id)) PARTITION BY LIST (tenant_id);
CREATE UNIQUE INDEX books_folder_id_title_key ON public.books (tenant_id, folder_id, title) WHERE note IS NULL;
ALTER TABLE public.books ENABLE ROW LEVEL SECURITY; ALTER TABLE public.books FORCE ROW LEVEL SECURITY;
CREATE POLICY stego_tenant ON public.books USING ("tenant_id" = pg_catalog.current_setting('app.tenant', true)) WITH CHECK ("tenant_id" = pg_catalog.current_setting('app.tenant', true));
CREATE TRIGGER books_reject_row BEFORE UPDATE OR DELETE ON public.books FOR EACH ROW EXECUTE FUNCTION public.books_reject();
CREATE TABLE public.books_all PARTITION OF public.books DEFAULT;
CREATE TRIGGER books_all_mark BEFORE TRUNCATE ON public.books_all FOR EACH STATEMENT EXECUTE FUNCTION public.books_reject();
`

// partitionedWant is the manifest with books partitioned: each partition has the clone of books_reject_row and a
// statement trigger of its own, <partition>_mark.
func partitionedWant() Manifest {
	m := want()
	m.Tables[1].Partitioned = true
	m.Tables[1].PartitionTriggers = []Trigger{{Name: "_mark", Timing: "BEFORE", Events: []string{"TRUNCATE"},
		Function: m.Tables[1].Triggers[0].Function}}
	return m
}

// redTeam3 are the hostile changes to the partitions of a partitioned table of the second delta review of PR 218 at
// f43cc169 (P1), each after partitionedBooks.
var redTeam3 = []struct{ name, statement, fault string }{
	{"(a) a partition trigger that copies rows out",
		`CREATE TABLE public.leak (LIKE public.books);
CREATE FUNCTION public.copy_row() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN INSERT INTO public.leak SELECT NEW.*; RETURN NEW; END $$;
CREATE TRIGGER copy BEFORE INSERT ON public.books_all FOR EACH ROW EXECUTE FUNCTION public.copy_row()`,
		"table public.books: partition public.books_all: trigger copy is a trigger that the manifest does not name"},
	{"(b) a partition trigger that drops rows",
		`CREATE FUNCTION public.drop_row() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NULL; END $$;
CREATE TRIGGER a_drop BEFORE INSERT ON public.books_all FOR EACH ROW EXECUTE FUNCTION public.drop_row()`,
		"table public.books: partition public.books_all: trigger a_drop is a trigger that the manifest does not name"},
	{"(c) a partition of another owner",
		`CREATE ROLE manifest_part_owner NOLOGIN; ALTER TABLE public.books_all OWNER TO manifest_part_owner`,
		"table public.books: partition public.books_all: the partition belongs to the role manifest_part_owner, and the table belongs to %owner%"},
	{"(d) the cloned reject trigger disabled on the partition",
		`ALTER TABLE public.books_all DISABLE TRIGGER books_reject_row`,
		"table public.books: partition public.books_all: trigger books_reject_row is disabled"},
	{"(e) the partition readable by PUBLIC",
		`GRANT SELECT ON public.books_all TO PUBLIC`,
		"table public.books: partition public.books_all: the partition grants a privilege on it or on a column to another role than its owner"},
	{"the partition writable by the reader",
		`GRANT INSERT ON public.books_all TO %reader%`,
		"table public.books: partition public.books_all: the partition grants a privilege on it or on a column to another role than its owner"},
	{"a column of the partition readable by PUBLIC",
		`GRANT SELECT (title) ON public.books_all TO PUBLIC`,
		"table public.books: partition public.books_all: the partition grants a privilege on it or on a column to another role than its owner"},
	{"a rule on the partition",
		`CREATE TABLE public.leak (LIKE public.books); CREATE RULE books_all_leak AS ON INSERT TO public.books_all DO ALSO INSERT INTO public.leak VALUES (NEW.*)`,
		"table public.books: partition public.books_all: the partition has a rule"},
	{"a partition that is partitioned again",
		`CREATE TABLE public.books_sub PARTITION OF public.books FOR VALUES IN ('t9') PARTITION BY LIST (tenant_id)`,
		"table public.books: partition public.books_sub: the partition is not a plain table partition"},
	{"the own statement trigger of the partition dropped",
		`DROP TRIGGER books_all_mark ON public.books_all`,
		"table public.books: partition public.books_all: trigger books_all_mark is missing"},
	{"the own statement trigger of the partition disabled",
		`ALTER TABLE public.books_all DISABLE TRIGGER books_all_mark`,
		"table public.books: partition public.books_all: trigger books_all_mark is disabled"},
}

func TestRedTeam3(t *testing.T) {
	t.Run("ST-SUP-RP15-LC01-T04 Check refuses a partition with another trigger, owner, rule or kind, or a disabled or missing trigger", func(t *testing.T) {
		ctx := context.Background()
		t.Run("the partitioned table as the manifest states it", func(t *testing.T) {
			admin, reader := database(t)
			if _, err := admin.Exec(partitionedBooks); err != nil {
				t.Fatal(err)
			}
			if err := Check(ctx, reader, partitionedWant()); err != nil {
				t.Fatalf("Check refused the partitioned table of the manifest: %v", err)
			}
		})
		for _, change := range redTeam3 {
			t.Run(change.name, func(t *testing.T) {
				admin, reader := database(t)
				t.Cleanup(func() {
					_, _ = admin.Exec("DROP OWNED BY manifest_part_owner; DROP ROLE IF EXISTS manifest_part_owner")
				})
				if _, err := admin.Exec(partitionedBooks); err != nil {
					t.Fatal(err)
				}
				var role string
				if err := reader.QueryRow("SELECT current_user").Scan(&role); err != nil {
					t.Fatal(err)
				}
				if _, err := admin.Exec(strings.ReplaceAll(change.statement, "%reader%", role)); err != nil {
					t.Fatal(err)
				}
				err := Check(ctx, reader, partitionedWant())
				fault := strings.ReplaceAll(change.fault, "%owner%", testOwner)
				var mismatch *Mismatch
				if !errors.As(err, &mismatch) || !slices.Contains(mismatch.Faults, fault) {
					t.Fatalf("Check gave %v, want the fault %q", err, fault)
				}
			})
		}
	})
}

func TestFunctions(t *testing.T) {
	t.Run("ST-SUP-RP15-LC01-T05 Check refuses a function of the manifest that is missing or has another source, SECURITY DEFINER, setting or owner", func(t *testing.T) {
		ctx := context.Background()
		const make = "CREATE FUNCTION public.books_maintain() RETURNS integer LANGUAGE plpgsql SECURITY DEFINER " +
			"SET search_path = pg_catalog, pg_temp SET lock_timeout = '2s' AS $$ BEGIN RETURN 1; END $$; " +
			"REVOKE ALL ON FUNCTION public.books_maintain() FROM PUBLIC"
		functions := []Function{{Name: "public.books_maintain", Language: "plpgsql", Source: " BEGIN RETURN 1; END ", SecurityDefiner: true,
			Config: []string{"search_path=pg_catalog, pg_temp", "lock_timeout=2s"}, Returns: "integer"}}
		other := fmt.Sprintf("manifest_other_%d", time.Now().UnixNano())
		for _, change := range []struct{ name, statement, fault string }{
			{"a missing function", "DROP FUNCTION public.books_maintain()", "function public.books_maintain is missing"},
			{"another source", "CREATE OR REPLACE FUNCTION public.books_maintain() RETURNS integer LANGUAGE plpgsql SECURITY DEFINER " +
				"SET search_path = pg_catalog, pg_temp SET lock_timeout = '2s' AS $$ BEGIN RETURN 2; END $$",
				"function public.books_maintain is not the function of the manifest"},
			{"SECURITY INVOKER", "ALTER FUNCTION public.books_maintain() SECURITY INVOKER", "function public.books_maintain is not the function of the manifest"},
			{"another setting", "ALTER FUNCTION public.books_maintain() SET search_path = public", "function public.books_maintain is not the function of the manifest"},
			{"another owner", "CREATE ROLE " + other + " NOLOGIN; ALTER FUNCTION public.books_maintain() OWNER TO " + other,
				"function public.books_maintain has another owner"},
			{"EXECUTE for PUBLIC", "GRANT EXECUTE ON FUNCTION public.books_maintain() TO PUBLIC",
				"function public.books_maintain may be run by roles that the manifest does not name"},
			{"EXECUTE for another role", "CREATE ROLE " + other + " NOLOGIN; GRANT EXECUTE ON FUNCTION public.books_maintain() TO " + other,
				"function public.books_maintain may be run by roles that the manifest does not name"},
			{"no EXECUTE for the role of the manifest", "", "function public.books_maintain may be run by roles that the manifest does not name"},
			{"EXECUTE with the grant option", "GRANT EXECUTE ON FUNCTION public.books_maintain() TO " + "%role%" + " WITH GRANT OPTION",
				"function public.books_maintain may be run by roles that the manifest does not name"},
			{"STRICT", "ALTER FUNCTION public.books_maintain() STRICT",
				"function public.books_maintain has another strictness, volatility or result than the manifest"},
			{"STABLE", "ALTER FUNCTION public.books_maintain() STABLE",
				"function public.books_maintain has another strictness, volatility or result than the manifest"},
			{"another result", "DROP FUNCTION public.books_maintain(); CREATE FUNCTION public.books_maintain() RETURNS bigint LANGUAGE plpgsql SECURITY DEFINER " +
				"SET search_path = pg_catalog, pg_temp SET lock_timeout = '2s' AS $$ BEGIN RETURN 1; END $$; " +
				"REVOKE ALL ON FUNCTION public.books_maintain() FROM PUBLIC; GRANT EXECUTE ON FUNCTION public.books_maintain() TO %role%",
				"function public.books_maintain has another strictness, volatility or result than the manifest"},
		} {
			t.Run(change.name, func(t *testing.T) {
				admin, reader := database(t)
				// The owner of the manifest is known once the database is made.
				m := want()
				m.Functions = slices.Clone(functions)
				var role string
				if err := reader.QueryRow("SELECT current_user").Scan(&role); err != nil {
					t.Fatal(err)
				}
				// EXECUTE goes to the reader of the test only, as the runtime role of a service.
				grant := "GRANT EXECUTE ON FUNCTION public.books_maintain() TO " + role
				if change.statement == "" {
					grant = "SELECT 1"
				}
				m.Functions[0].Execute = []string{role}
				if m.Owner == "" {
					t.Fatal("the test database has no owner")
				}
				if _, err := admin.Exec(make); err != nil {
					t.Fatal(err)
				}
				if _, err := admin.Exec(grant); err != nil {
					t.Fatal(err)
				}
				if change.statement != "" {
					if err := Check(ctx, reader, m); err != nil {
						t.Fatalf("Check refused the database of the manifest: %v", err)
					}
					if _, err := admin.Exec(strings.ReplaceAll(change.statement, "%role%", role)); err != nil {
						t.Fatal(err)
					}
				}
				t.Cleanup(func() {
					_, _ = admin.Exec("DROP FUNCTION IF EXISTS public.books_maintain()")
					_, _ = admin.Exec("DROP ROLE IF EXISTS " + other)
				})
				err := Check(ctx, reader, m)
				var mismatch *Mismatch
				if !errors.As(err, &mismatch) || len(mismatch.Faults) != 1 || mismatch.Faults[0] != change.fault {
					t.Fatalf("Check gave %v, want the one fault %q", err, change.fault)
				}
			})
		}
	})
}

func TestValidateNewFields(t *testing.T) {
	t.Run("ST-SUP-RP15-LC01-T06 A manifest that names PUBLIC, grants it does not check, or a bad role, privilege or argument list is refused", func(t *testing.T) {
		table := func(change func(*Table)) Manifest {
			tb := Table{Schema: "public", Name: "t", CheckGrants: true}
			change(&tb)
			return Manifest{Tables: []Table{tb}}
		}
		for name, m := range map[string]Manifest{
			"a policy for PUBLIC by name":     table(func(t *Table) { t.Policies = []Policy{{Name: "p", Command: "*", Roles: []string{"PUBLIC"}}} }),
			"grants that are not checked":     table(func(t *Table) { t.CheckGrants = false; t.Grants = []Grant{{Role: "r", Privileges: []string{"SELECT"}}} }),
			"a grant to PUBLIC":               table(func(t *Table) { t.Grants = []Grant{{Role: "PUBLIC", Privileges: []string{"SELECT"}}} }),
			"a grant of no privilege":         table(func(t *Table) { t.Grants = []Grant{{Role: "r"}} }),
			"a grant of an unknown privilege": table(func(t *Table) { t.Grants = []Grant{{Role: "r", Privileges: []string{"ALL"}}} }),
			"EXECUTE for PUBLIC":              {Functions: []Function{{Name: "public.f", Language: "sql", Source: "SELECT 1", Execute: []string{"PUBLIC"}}}},
			"an argument list with SQL text":  {Functions: []Function{{Name: "public.f", Language: "sql", Source: "SELECT 1", Returns: "integer", Args: "int); DROP TABLE x; --"}}},
			"a function with no result":       {Functions: []Function{{Name: "public.f", Language: "sql", Source: "SELECT 1"}}},
			"an unknown volatility":           {Functions: []Function{{Name: "public.f", Language: "sql", Source: "SELECT 1", Returns: "integer", Volatility: "x"}}},
			"a schema with no owner":          {Schemas: []Schema{{Name: "held"}}},
			"a schema for PUBLIC":             {Owner: "o", Schemas: []Schema{{Name: "held", Usage: []string{"PUBLIC"}}}},
			"a schema named twice":            {Owner: "o", Schemas: []Schema{{Name: "held"}, {Name: "held"}}},
			"a shared schema with USAGE":      {Owner: "o", Schemas: []Schema{{Name: "public", Shared: true, Usage: []string{"r"}}}},
			"PUBLIC confined":                 {Confined: []string{"PUBLIC"}},
		} {
			if err := m.Validate(); err == nil {
				t.Errorf("%s: Validate took it", name)
			}
		}
	})
}

func TestSchemas(t *testing.T) {
	t.Run("ST-SUP-RP15-LC01-T07 Check refuses a held schema with another function, owner or privilege, and a confined role that owns what the manifest does not name", func(t *testing.T) {
		ctx := context.Background()
		suffix := fmt.Sprintf("%d", time.Now().UnixNano())
		confined, other := "manifest_confined_"+suffix, "manifest_stranger_"+suffix
		function := Function{Name: "held.f", Args: "integer", Language: "sql", Source: "SELECT $1", SecurityDefiner: true,
			Config: []string{"search_path=pg_catalog, pg_temp"}, Owner: confined, Returns: "integer"}
		for _, change := range []struct{ name, statement, fault string }{
			{"an overload in the schema", "CREATE FUNCTION held.f(double precision) RETURNS integer LANGUAGE sql AS 'SELECT 1'",
				"schema held: function held.f(double precision) is a function that the manifest does not name"},
			{"another function in the schema", "CREATE FUNCTION held.g() RETURNS integer LANGUAGE sql AS 'SELECT 1'",
				"schema held: function held.g() is a function that the manifest does not name"},
			{"USAGE for PUBLIC", "GRANT USAGE ON SCHEMA held TO PUBLIC", "schema held: the privilege USAGE to PUBLIC is granted, and the manifest does not grant it"},
			{"CREATE for a role of the manifest", "GRANT CREATE ON SCHEMA held TO %role%",
				"schema held: the privilege CREATE to %role% is granted, and the manifest does not grant it"},
			{"USAGE with the grant option", "GRANT USAGE ON SCHEMA held TO %role% WITH GRANT OPTION",
				"schema held: the privilege USAGE to %role% is granted with the grant option"},
			{"no USAGE for a role of the manifest", "REVOKE USAGE ON SCHEMA held FROM " + confined,
				"schema held: the privilege USAGE to " + confined + " of the manifest is not granted"},
			{"the schema of another owner", "ALTER SCHEMA held OWNER TO " + other,
				"schema held: the schema belongs to the role " + other + ", and the manifest needs %owner%"},
			{"CREATE on a shared schema for a role", "GRANT CREATE ON SCHEMA public TO %role%",
				"schema public: the privilege CREATE to %role% is granted, and the manifest gives it to no role but the owner"},
			{"an overload of a function of the manifest outside a held schema", "CREATE FUNCTION public.books_reject(integer) RETURNS integer LANGUAGE sql AS 'SELECT 1'",
				"function public.books_reject(integer) has the name of a function of the manifest, and is not one of it"},
			{"a function of the confined role outside the manifest", "CREATE FUNCTION public.peek() RETURNS bigint LANGUAGE sql SECURITY DEFINER AS 'SELECT 1'; " +
				"ALTER FUNCTION public.peek() OWNER TO " + confined, "the confined role " + confined + " owns the function public.peek(), which the manifest does not name for it"},
			{"a table of the confined role", "CREATE TABLE public.side (x integer); ALTER TABLE public.side OWNER TO " + confined,
				"the confined role " + confined + " owns the relation public.side, which the manifest does not name for it"},
			{"a type of the confined role", "CREATE TYPE public.mood AS ENUM ('calm'); ALTER TYPE public.mood OWNER TO " + confined,
				"the confined role " + confined + " owns the type public.mood, which the manifest does not name for it"},
			{"a schema of the confined role", "CREATE SCHEMA side AUTHORIZATION " + confined,
				"the confined role " + confined + " owns the schema side, which the manifest does not name for it"},
			{"a large object of the confined role", "SELECT lo_create(424242); ALTER LARGE OBJECT 424242 OWNER TO " + confined,
				"the confined role " + confined + " owns the large object 424242, which the manifest does not name for it"},
			{"an operator of the confined role", "CREATE OPERATOR public.=== (LEFTARG = integer, RIGHTARG = integer, FUNCTION = int4eq); " +
				"ALTER OPERATOR public.=== (integer, integer) OWNER TO " + confined,
				"the confined role " + confined + " owns the operator public.===(integer,integer), which the manifest does not name for it"},
			{"a collation of the confined role", "CREATE COLLATION public.side_c (locale = 'C'); ALTER COLLATION public.side_c OWNER TO " + confined,
				"the confined role " + confined + " owns the collation public.side_c, which the manifest does not name for it"},
			{"a text search configuration of the confined role", "CREATE TEXT SEARCH CONFIGURATION public.side_ts (COPY = simple); " +
				"ALTER TEXT SEARCH CONFIGURATION public.side_ts OWNER TO " + confined,
				"the confined role " + confined + " owns the text search configuration public.side_ts, which the manifest does not name for it"},
			{"default privileges of the confined role", "ALTER DEFAULT PRIVILEGES FOR ROLE " + confined + " GRANT EXECUTE ON FUNCTIONS TO " + other,
				"the confined role " + confined + " owns the default privileges for f, which the manifest does not name for it"},
			{"a membership of the confined role", "GRANT pg_read_all_data TO " + confined,
				"the confined role " + confined + " is a member of the role pg_read_all_data, which the manifest does not name for it"},
			{"a member of the confined role besides the owner", "GRANT " + confined + " TO %owner% WITH SET TRUE, INHERIT FALSE; GRANT " + confined + " TO " + other,
				"the role " + other + " is a member of the confined role " + confined + ", which the manifest grants only to its owner WITH SET TRUE, INHERIT FALSE and no ADMIN"},
			{"a member with ADMIN that a superuser granted", "GRANT " + confined + " TO " + other + " WITH ADMIN TRUE, INHERIT FALSE",
				"the role " + other + " is a member of the confined role " + confined + ", which the manifest grants only to its owner WITH SET TRUE, INHERIT FALSE and no ADMIN; " +
					"a superuser runs REVOKE " + confined + " FROM " + other + " GRANTED BY %owner%, or the role migration runs as a superuser"},
			{"the owner as a member that inherits", "GRANT " + confined + " TO %owner% WITH SET TRUE, INHERIT TRUE",
				"the role %owner% is a member of the confined role " + confined + ", which the manifest grants only to its owner WITH SET TRUE, INHERIT FALSE and no ADMIN"},
			{"a privilege of the confined role on another table", "CREATE TABLE public.side (x integer); GRANT SELECT (x) ON public.side TO " + confined,
				"the confined role " + confined + " holds a privilege on the relation public.side, which the manifest does not name for it"},
		} {
			t.Run(change.name, func(t *testing.T) {
				admin, reader := database(t)
				var role string
				if err := reader.QueryRow("SELECT current_user").Scan(&role); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					// One role at a time: DROP OWNED BY of two roles fails on default privileges of one for the other.
					_, _ = admin.Exec("DROP OWNED BY " + confined)
					_, _ = admin.Exec("DROP OWNED BY " + other)
					_, _ = admin.Exec("DROP ROLE IF EXISTS " + confined)
					_, _ = admin.Exec("DROP ROLE IF EXISTS " + other)
				})
				if _, err := admin.Exec("CREATE ROLE " + confined + " NOLOGIN; CREATE ROLE " + other + " NOLOGIN; CREATE SCHEMA held; " +
					"CREATE FUNCTION held.f(integer) RETURNS integer LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS 'SELECT $1'; " +
					"REVOKE ALL ON FUNCTION held.f(integer) FROM PUBLIC; ALTER FUNCTION held.f(integer) OWNER TO " + confined + "; " +
					"GRANT USAGE ON SCHEMA held TO " + role + ", " + confined); err != nil {
					t.Fatal(err)
				}
				m := want()
				m.Functions = []Function{function}
				m.Schemas = []Schema{{Name: "held", Usage: []string{role, confined}}, {Name: "public", Shared: true}}
				m.Confined = []string{confined}
				if err := Check(ctx, reader, m); err != nil {
					t.Fatalf("Check refused the database of the manifest: %v", err)
				}
				if _, err := admin.Exec(strings.NewReplacer("%role%", role, "%owner%", m.Owner).Replace(change.statement)); err != nil {
					t.Fatal(err)
				}
				want := strings.NewReplacer("%role%", role, "%owner%", m.Owner).Replace(change.fault)
				err := Check(ctx, reader, m)
				var mismatch *Mismatch
				if !errors.As(err, &mismatch) || len(mismatch.Faults) != 1 || mismatch.Faults[0] != want {
					t.Fatalf("Check gave %v, want the one fault %q", err, want)
				}
			})
		}
	})
}

func TestSearchPath(t *testing.T) {
	t.Run("ST-SUP-RP15-LC01-T08 Check refuses a relation or a function of the name of one of the manifest earlier on the search path of the session, and a schema named after its role", func(t *testing.T) {
		ctx := context.Background()
		for _, change := range []struct {
			name, statement string
			faults          []string
		}{
			{"a schema named after the role", "CREATE SCHEMA %role%",
				[]string{"the schema %role% has the name of the role of the session, which a search path of \"$user\" puts first"}},
			{"a shadow table in the schema of the role", "CREATE SCHEMA %role%; GRANT USAGE ON SCHEMA %role% TO %role%; CREATE TABLE %role%.books (tenant_id text)",
				[]string{"the schema %role% has the name of the role of the session, which a search path of \"$user\" puts first",
					"the relation %role%.books comes before public.books on the search path of the session"}},
			{"a view in an earlier schema of the path", "CREATE SCHEMA early; GRANT USAGE ON SCHEMA early TO %role%; ALTER ROLE %role% SET search_path = early, public; " +
				"CREATE VIEW early.folders AS SELECT 1 AS x",
				[]string{"the relation early.folders comes before public.folders on the search path of the session"}},
			{"a function in an earlier schema of the path", "CREATE SCHEMA early; GRANT USAGE ON SCHEMA early TO %role%; ALTER ROLE %role% SET search_path = early, public; " +
				"CREATE FUNCTION early.books_maintain() RETURNS integer LANGUAGE sql AS 'SELECT 2'",
				[]string{"the function early.books_maintain comes before public.books_maintain on the search path of the session"}},
		} {
			t.Run(change.name, func(t *testing.T) {
				admin, reader := database(t)
				var role string
				if err := reader.QueryRow("SELECT current_user").Scan(&role); err != nil {
					t.Fatal(err)
				}
				if _, err := admin.Exec("CREATE FUNCTION public.books_maintain() RETURNS integer LANGUAGE sql AS 'SELECT 1'; " +
					"REVOKE ALL ON FUNCTION public.books_maintain() FROM PUBLIC; GRANT EXECUTE ON FUNCTION public.books_maintain() TO " + role); err != nil {
					t.Fatal(err)
				}
				m := want()
				m.Functions = []Function{{Name: "public.books_maintain", Language: "sql", Source: "SELECT 1", Returns: "integer", Execute: []string{role}}}
				if err := Check(ctx, reader, m); err != nil {
					t.Fatalf("Check refused the database of the manifest: %v", err)
				}
				if _, err := admin.Exec(strings.ReplaceAll(change.statement, "%role%", role)); err != nil {
					t.Fatal(err)
				}
				// A setting of the role takes effect on a new session.
				reader.SetMaxIdleConns(0)
				var wantFaults []string
				for _, f := range change.faults {
					wantFaults = append(wantFaults, strings.ReplaceAll(f, "%role%", role))
				}
				err := Check(ctx, reader, m)
				var mismatch *Mismatch
				if !errors.As(err, &mismatch) || !slices.Equal(mismatch.Faults, wantFaults) {
					t.Fatalf("Check gave %v, want the faults %q", err, wantFaults)
				}
			})
		}
	})
}

func TestSearchPathFunctions(t *testing.T) {
	t.Run("ST-SUP-RP15-LC01-T08 A function of a manifest name in a schema of the path is no fault when the schema of the manifest function is not on the path", func(t *testing.T) {
		ctx := context.Background()
		admin, reader := database(t)
		var role string
		if err := reader.QueryRow("SELECT current_user").Scan(&role); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.Exec("CREATE SCHEMA held; GRANT USAGE ON SCHEMA held TO " + role + "; " +
			"CREATE FUNCTION held.claim() RETURNS integer LANGUAGE sql AS 'SELECT 1'; REVOKE ALL ON FUNCTION held.claim() FROM PUBLIC; " +
			"CREATE FUNCTION public.claim() RETURNS integer LANGUAGE sql AS 'SELECT 2'"); err != nil {
			t.Fatal(err)
		}
		m := want()
		m.Functions = []Function{{Name: "held.claim", Language: "sql", Source: "SELECT 1", Returns: "integer"}}
		if err := Check(ctx, reader, m); err != nil {
			t.Fatalf("Check refused a function of the same name in public, which no call of held.claim reaches: %v", err)
		}
		// With held on the path, a function of its name in a schema before held is a fault (#245 Q3).
		if _, err := admin.Exec("CREATE SCHEMA early; GRANT USAGE ON SCHEMA early TO " + role + "; ALTER ROLE " + role + " SET search_path = early, held, public; " +
			"CREATE FUNCTION early.claim() RETURNS integer LANGUAGE sql AS 'SELECT 3'"); err != nil {
			t.Fatal(err)
		}
		reader.SetMaxIdleConns(0)
		err := Check(ctx, reader, m)
		var mismatch *Mismatch
		want := "the function early.claim comes before held.claim on the search path of the session"
		if !errors.As(err, &mismatch) || len(mismatch.Faults) != 1 || mismatch.Faults[0] != want {
			t.Fatalf("Check gave %v, want the one fault %q", err, want)
		}
	})
}
