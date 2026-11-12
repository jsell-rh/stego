package manifest

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"

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
func want() Manifest {
	return Manifest{Tables: []Table{
		{Schema: "public", Name: "folders", Columns: []Column{{Name: "tenant_id", Type: "text", NotNull: true}, {Name: "id", Type: "uuid", NotNull: true}},
			Constraints: []Constraint{{Name: "folders_pkey", Kind: KindPrimaryKey}},
			Indexes:     []Index{{Name: "folders_pkey", Columns: []string{"tenant_id", "id"}}}},
		{Schema: "public", Name: "books",
			Columns: []Column{{Name: "tenant_id", Type: "text", NotNull: true}, {Name: "id", Type: "uuid", NotNull: true},
				{Name: "folder_id", Type: "uuid", NotNull: true}, {Name: "title", Type: "character varying(255)", NotNull: true},
				{Name: "note", Type: "text"}, {Name: "pages", Type: "integer"}},
			Constraints: []Constraint{{Name: "books_pkey", Kind: KindPrimaryKey}, {Name: "books_title_check", Kind: KindCheck},
				{Name: "books_folder_id_fkey", Kind: KindForeignKey, References: "public.folders"}},
			Indexes: []Index{{Name: "books_pkey", Columns: []string{"tenant_id", "id"}},
				{Name: "books_folder_id_title_key", Columns: []string{"tenant_id", "folder_id", "title"}, Predicate: "(note IS NULL)"}},
			RowSecurity: true,
			Policies:    []Policy{{Name: "stego_tenant", Command: "*", Using: tenantWall, WithCheck: tenantWall}},
			Triggers: []Trigger{{Name: "books_reject_row", Timing: "BEFORE", Row: true, Events: []string{"UPDATE", "DELETE"},
				Function: "public.books_reject"}}},
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
			{"a changed column type", "ALTER TABLE public.books ALTER COLUMN title TYPE text",
				"table public.books: column title has the type text, and the manifest needs character varying(255)"},
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
			{"an added permissive policy", "CREATE POLICY open_door ON public.books USING (true)",
				"table public.books: policy open_door is a permissive policy that the manifest does not name"},
			{"a dropped trigger", "DROP TRIGGER books_reject_row ON public.books", "table public.books: trigger books_reject_row is missing"},
			{"a disabled trigger", "ALTER TABLE public.books DISABLE TRIGGER books_reject_row",
				"table public.books: trigger books_reject_row is disabled"},
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

		t.Run("more objects are allowed, and a nullable column may be NOT NULL", func(t *testing.T) {
			admin, reader := database(t)
			for _, statement := range []string{
				"ALTER TABLE public.books ADD COLUMN extra text",
				"CREATE INDEX books_extra ON public.books (extra)",
				"CREATE POLICY narrow ON public.books AS RESTRICTIVE USING (true)",
				"CREATE TRIGGER books_more AFTER INSERT ON public.books FOR EACH ROW EXECUTE FUNCTION public.books_reject()",
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
				{Tables: []Table{{Schema: "public", Name: "a", Indexes: []Index{{Name: "i"}}}}},
				{Tables: []Table{{Schema: "public", Name: "a", Policies: []Policy{{Name: "p", Command: "x"}}}}},
				{Tables: []Table{{Schema: "public", Name: "a", Triggers: []Trigger{{Name: "t", Timing: "BEFORE", Function: "f"}}}}},
				{Tables: []Table{{Schema: "public", Name: "a", Triggers: []Trigger{{Name: "t", Timing: "SOON", Events: []string{"INSERT"}, Function: "f"}}}}},
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
