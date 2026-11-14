package manifest

// The step forms that Check accepts on an entity table and the partition objects it refuses, case by case, from the
// independent verification of PR #270 (ST-SUP-RP15-LC01-T12 to T14). Each case sets up a schema state on a new
// database of the test server and asserts the result of Check.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jsell-rh/stego/registry/runtime/internal/pgtest"
)

const formLabel = `ALTER TABLE public.books ADD COLUMN "label" text;` + "\n"

func formCheck(name, expr string) string {
	return `ALTER TABLE public.books ADD CONSTRAINT "` + name + `" CHECK (` + expr + `);` + "\n"
}

// formFaults runs setup and gives the faults of Check against m (nil when Check accepts).
func formFaults(t *testing.T, admin, reader *sql.DB, setup string, m Manifest) []string {
	t.Helper()
	if setup != "" {
		if _, err := admin.Exec(setup); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
	m.Owner = testOwner
	err := Check(context.Background(), reader, m)
	if err == nil {
		return nil
	}
	var mismatch *Mismatch
	if !errors.As(err, &mismatch) {
		t.Fatalf("Check gives %v, want nil or a mismatch", err)
	}
	return mismatch.Faults
}

func TestStepCheckForms(t *testing.T) {
	t.Run("ST-SUP-RP15-LC01-T12 acceptCheck refuses each hostile CHECK form", func(t *testing.T) {
		const userFns = hostileFunctions +
			"CREATE FUNCTION public.t2b(text) RETURNS boolean LANGUAGE plpgsql IMMUTABLE AS $e$ BEGIN RAISE EXCEPTION 'EVIL RAN'; END $e$;\n" +
			"CREATE FUNCTION public.ge(text, integer) RETURNS boolean LANGUAGE sql IMMUTABLE AS $e$ SELECT true $e$;\n"
		cases := []struct{ name, setup string }{
			// A user function, directly or through an operator, a cast, a domain or a collation.
			{"a user function", userFns + formLabel + formCheck("books_label_check", `public.evil("label")`)},
			{"a user function inside AND with a step form", userFns + formLabel + formCheck("books_label_check", `char_length("label") >= 1 AND public.evil("label")`)},
			{"a user function as an argument of a built-in", userFns + formLabel + formCheck("books_label_check", `char_length(public.evilt("label")) >= 1`)},
			{"a user operator on a user function", userFns + "CREATE OPERATOR public.>>> (LEFTARG = text, RIGHTARG = integer, FUNCTION = public.ge);\n" + formLabel +
				formCheck("books_label_check", `"label" OPERATOR(public.>>>) 1`)},
			{"a user operator on a built-in function", "CREATE OPERATOR public.=== (LEFTARG = text, RIGHTARG = text, FUNCTION = pg_catalog.texteq);\n" + formLabel +
				formCheck("books_label_check", `"label" OPERATOR(public.===) 'a'`)},
			{"a user operator in ANY", "CREATE OPERATOR public.=== (LEFTARG = text, RIGHTARG = text, FUNCTION = pg_catalog.texteq);\n" + formLabel +
				formCheck("books_label_check", `"label" OPERATOR(public.===) ANY (ARRAY['a', 'b'])`)},
			{"a user cast between built-in types", userFns + "CREATE CAST (text AS boolean) WITH FUNCTION public.t2b(text);\n" + formLabel +
				formCheck("books_label_check", `("label")::boolean`)},
			{"a cast to a user domain", "CREATE DOMAIN public.short AS text CHECK (char_length(VALUE) < 9);\n" + formLabel +
				formCheck("books_label_check", `("label")::public.short IS NOT NULL`)},
			{"a cast to a user enum", "CREATE TYPE public.mood AS ENUM ('a', 'b');\n" + formLabel +
				formCheck("books_label_check", `("label")::public.mood IS NOT NULL`)},
			{"a binary cast to a user domain over text", "CREATE DOMAIN public.dt AS text;\n" + formLabel +
				formCheck("books_label_check", `("label")::public.dt <> 'x'`)},
			{"a constant of a user enum", "CREATE TYPE public.mood AS ENUM ('a', 'b');\n" + formLabel +
				formCheck("books_label_check", `"label" IS NOT NULL OR 'a'::public.mood IS NOT NULL`)},
			{"a user collation in COLLATE", "CREATE COLLATION public.c2 (provider = libc, locale = 'C');\n" + formLabel +
				formCheck("books_label_check", `"label" COLLATE public.c2 > 'a'`)},
			{"a column of a user collation", "CREATE COLLATION public.c2 (provider = libc, locale = 'C');\n" +
				`ALTER TABLE public.books ADD COLUMN "label" text COLLATE public.c2;` + "\n" + formCheck("books_label_check", `"label" > 'a'`)},
			// A CASE, other expression nodes, and a type coercion through text.
			{"a CASE", formLabel + formCheck("books_label_check", `CASE WHEN "label" IS NULL THEN true ELSE char_length("label") >= 1 END`)},
			{"a COALESCE", formLabel + formCheck("books_label_check", `COALESCE(char_length("label"), 1) >= 1`)},
			{"a NULLIF", formLabel + formCheck("books_label_check", `NULLIF("label", 'x') IS NOT NULL`)},
			{"a GREATEST", formLabel + formCheck("books_label_check", `GREATEST(char_length("label"), 0) >= 1`)},
			{"an IS TRUE", formLabel + formCheck("books_label_check", `(char_length("label") >= 1) IS TRUE`)},
			{"an IS DISTINCT FROM", formLabel + formCheck("books_label_check", `"label" IS DISTINCT FROM 'x'`)},
			{"a ROW", formLabel + formCheck("books_label_check", `ROW("label") IS NOT NULL`)},
			{"an integer through text", `ALTER TABLE public.books ADD COLUMN "rank" integer;` + "\n" + formCheck("books_rank_check", `("rank")::text <> 'x'`)},
			{"text to json", formLabel + formCheck("books_label_check", `("label")::json IS NOT NULL`)},
			{"text to regclass", formLabel + formCheck("books_label_check", `("label")::regclass IS NOT NULL`)},
			{"text to regproc", formLabel + formCheck("books_label_check", `("label")::regproc IS NOT NULL`)},
			{"a regclass constant of a user table", "CREATE TABLE public.other (x int);\n" + formLabel +
				formCheck("books_label_check", `"label" IS NOT NULL OR 'public.other'::regclass IS NOT NULL`)},
			{"a stable operator: timestamptz against date", `ALTER TABLE public.books ADD COLUMN "at" timestamptz;` + "\n" +
				formCheck("books_at_check", `"at" > '2020-01-01'::date`)},
			{"a stable function: to_char", `ALTER TABLE public.books ADD COLUMN "at" timestamptz;` + "\n" +
				formCheck("books_at_check", `to_char("at", 'YYYY') <> ''`)},
			// A volatile or stable built-in.
			{"random", formLabel + formCheck("books_label_check", `char_length("label") >= 1 AND random() < 2`)},
			{"clock_timestamp", `ALTER TABLE public.books ADD COLUMN "at" timestamptz;` + "\n" + formCheck("books_at_check", `"at" <= clock_timestamp()`)},
			{"now", `ALTER TABLE public.books ADD COLUMN "at" timestamptz;` + "\n" + formCheck("books_at_check", `"at" <= now()`)},
			{"nextval", "CREATE SEQUENCE public.s;\n" + `ALTER TABLE public.books ADD COLUMN "rank" bigint;` + "\n" + formCheck("books_rank_check", `"rank" <> nextval('public.s')`)},
			{"set_config", formLabel + formCheck("books_label_check", `set_config('app.tenant', 'other', false) IS NOT NULL OR "label" IS NULL`)},
			{"current_setting", formLabel + formCheck("books_label_check", `"label" <> current_setting('app.tenant', true)`)},
			{"pg_sleep", formLabel + formCheck("books_label_check", `"label" IS NULL OR pg_sleep(0) IS NULL`)},
			{"txid_current", `ALTER TABLE public.books ADD COLUMN "rank" bigint;` + "\n" + formCheck("books_rank_check", `"rank" <> txid_current()`)},
			{"pg_advisory_lock", `ALTER TABLE public.books ADD COLUMN "rank" bigint;` + "\n" + formCheck("books_rank_check", `"rank" IS NULL OR pg_try_advisory_lock("rank")`)},
			// Another name.
			{"another name", formLabel + formCheck("books_label_len", `char_length("label") >= 1`)},
			{"the name of another column", formLabel + formCheck("books_note_check", `char_length("label") >= 1`)},
			// Two columns, a system column, the whole row.
			{"two columns", formLabel + formCheck("books_label_check", `char_length("label") >= 1 AND "label" <> "note"`)},
			{"two unlisted columns", formLabel + `ALTER TABLE public.books ADD COLUMN "label2" text;` + "\n" + formCheck("books_label_check", `"label" <> "label2"`)},
			{"a system column", formLabel + formCheck("books_label_check", `tableoid::oid > 0 AND "label" IS NOT NULL`)},
			{"the whole row", formLabel + formCheck("books_label_check", `books IS NOT NULL AND "label" IS NOT NULL`)},
			{"no column", formLabel + formCheck("books_label_check", `true`)},
			// NO INHERIT.
			{"NO INHERIT", formLabel + `ALTER TABLE public.books ADD CONSTRAINT "books_label_check" CHECK (char_length("label") >= 1) NO INHERIT;` + "\n"},
			{"NO INHERIT NOT VALID", formLabel + `ALTER TABLE public.books ADD CONSTRAINT "books_label_check" CHECK (char_length("label") >= 1) NO INHERIT NOT VALID;` + "\n"},
			// A column of a type that is not built in.
			{"a column of a domain", "CREATE DOMAIN public.dom AS text;\nALTER TABLE public.books ADD COLUMN \"label\" public.dom;\n" + formCheck("books_label_check", `char_length("label") >= 1`)},
			{"a listed column", formCheck("books_pages_check", `"pages" >= 0`)},
		}
		for _, c := range cases {
			stepCase(t, c.name, c.setup, stepManifest(), true)
		}
		// A binary cast to a user enum is refused by PostgreSQL.
		t.Run("a binary cast to a user enum is refused by PostgreSQL", func(t *testing.T) {
			admin, _ := database(t)
			if _, err := admin.Exec("CREATE TYPE public.mood AS ENUM ('a', 'b');\nCREATE CAST (text AS public.mood) WITHOUT FUNCTION;\n"); err == nil {
				t.Error("PostgreSQL makes a binary cast from text to an enum")
			}
		})
		// A subquery is no CHECK that PostgreSQL makes, so the state cannot be set up.
		t.Run("a subquery is refused by PostgreSQL", func(t *testing.T) {
			admin, _ := database(t)
			_, err := admin.Exec(formLabel + formCheck("books_label_check", `"label" IN (SELECT 'a')`))
			if err == nil || !strings.Contains(err.Error(), "subquer") {
				t.Errorf("a CHECK with a subquery gives %v, want PostgreSQL to refuse it", err)
			}
		})
	})

	t.Run("ST-SUP-RP15-LC01-T12 acceptCheck accepts each form the generator emits", func(t *testing.T) {
		forms := []struct{ name, column, check string }{
			{"min length text", `"label" text`, `char_length("label") >= 1`},
			{"min length varchar", `"label" character varying(40)`, `char_length("label") >= 2`},
			{"integer bounds", `"label" integer`, `"label" >= 0 AND "label" <= 10`},
			{"bigint bound", `"label" bigint`, `"label" >= 1`},
			{"float bounds and finite", `"label" double precision`, `"label" >= 0 AND "label" <= 1.5 AND "label" NOT IN ('NaN'::double precision, 'Infinity'::double precision, '-Infinity'::double precision)`},
			{"real bound and finite", `"label" real`, `"label" <= 2 AND "label" NOT IN ('NaN'::double precision, 'Infinity'::double precision, '-Infinity'::double precision)`},
			{"enum values", `"label" text`, `"label" IN ('calm', 'busy')`},
			{"one enum value", `"label" text`, `"label" IN ('calm')`},
		}
		for _, f := range forms {
			setup := `ALTER TABLE public.books ADD COLUMN ` + f.column + ";\n"
			stepCase(t, "validated: "+f.name, setup+formCheck("books_label_check", f.check), stepManifest(), false)
			stepCase(t, "NOT VALID: "+f.name, setup+`ALTER TABLE public.books ADD CONSTRAINT "books_label_check" CHECK (`+f.check+`) NOT VALID;`+"\n", stepManifest(), false)
		}
	})
}

// formListed is stepManifest with the column label of type typ listed, as the release that names the step gives it,
// and with the CHECK books_label_check named by the definition def when def is not empty.
func formListed(typ, def string) Manifest {
	m := stepManifest()
	m.Tables[1].Columns = append(m.Tables[1].Columns, Column{Name: "label", Type: typ})
	if def != "" {
		m.Tables[1].Constraints = append(m.Tables[1].Constraints, Constraint{Name: "books_label_check", Kind: KindCheck, Definition: def})
	}
	return m
}

func TestStepNotValid(t *testing.T) {
	t.Run("ST-SUP-RP15-LC01-T12 NOT VALID is accepted by the release before and refused by the release that lists the column", func(t *testing.T) {
		const notValid = formLabel + `ALTER TABLE public.books ADD CONSTRAINT "books_label_check" CHECK (char_length("label") >= 1) NOT VALID;` + "\n"
		const def = "CHECK ((char_length(label) >= 1))"
		cases := []struct {
			name, setup string
			m           Manifest
			fault       string
		}{
			{"the release before, NOT VALID", notValid, stepManifest(), ""},
			{"the release that lists the column and names the CHECK, NOT VALID", notValid, formListed("text", def), "table public.books: constraint books_label_check is not validated"},
			{"the release that lists the column and does not name the CHECK, NOT VALID", notValid, formListed("text", ""), "table public.books: constraint books_label_check is a check constraint that the manifest does not name"},
			{"the release that lists the column, validated", notValid + "ALTER TABLE public.books VALIDATE CONSTRAINT books_label_check;\n", formListed("text", def), ""},
			{"the release before, validated", notValid + "ALTER TABLE public.books VALIDATE CONSTRAINT books_label_check;\n", stepManifest(), ""},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				admin, reader := database(t)
				faults := formFaults(t, admin, reader, c.setup, c.m)
				switch {
				case c.fault == "" && faults != nil:
					t.Errorf("Check refuses: %q", faults)
				case c.fault != "" && (len(faults) != 1 || faults[0] != c.fault):
					t.Errorf("Check gives %q, want %q", faults, c.fault)
				}
			})
		}
	})
}

func TestStepUnique(t *testing.T) {
	t.Run("ST-SUP-RP15-LC01-T13 the step unique index is unique within one tenant only", func(t *testing.T) {
		admin, reader := database(t)
		suffix := fmt.Sprintf("%d", time.Now().UnixNano())
		writer := "form_writer_" + suffix
		setup := `ALTER TABLE public.books ADD COLUMN "slug" text;
CREATE UNIQUE INDEX "books_slug_key" ON public."books" ("tenant_id", "slug");
DROP TRIGGER books_reject_row ON public.books;
INSERT INTO public.folders VALUES ('a', '00000000-0000-0000-0000-00000000000a'), ('b', '00000000-0000-0000-0000-00000000000b');
CREATE ROLE ` + writer + ` LOGIN PASSWORD 'w' NOSUPERUSER NOBYPASSRLS;
GRANT SELECT, INSERT ON public.books TO ` + writer + `;
`
		t.Cleanup(func() { _, _ = admin.Exec("DROP OWNED BY " + writer + "; DROP ROLE IF EXISTS " + writer) })
		m := stepManifest()
		m.Tables[1].Triggers = nil
		if faults := formFaults(t, admin, reader, setup, m); faults != nil {
			t.Fatalf("Check refuses the step unique index: %q", faults)
		}
		dsn, _ := url.Parse(pgDSN(t, admin))
		dsn.User = url.UserPassword(writer, "w")
		w, err := sql.Open("pgx", dsn.String())
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close()
		insert := func(tenant, id, slug string) error {
			ctx := context.Background()
			tx, err := w.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			if _, err := tx.Exec("SELECT set_config('app.tenant', $1, true)", tenant); err != nil {
				return err
			}
			if _, err := tx.Exec(`INSERT INTO public.books (id, folder_id, title, slug) VALUES ($1::uuid, $2, $1::text, $3)`, id,
				"00000000-0000-0000-0000-00000000000"+tenant, slug); err != nil {
				return err
			}
			return tx.Commit()
		}
		if err := insert("a", "00000000-0000-0000-0000-000000000001", "same"); err != nil {
			t.Fatalf("tenant a: %v", err)
		}
		if err := insert("b", "00000000-0000-0000-0000-000000000002", "same"); err != nil {
			t.Errorf("tenant b with the slug of tenant a is refused: %v", err)
		}
		err = insert("a", "00000000-0000-0000-0000-000000000003", "same")
		var pg *pgconn.PgError
		if !errors.As(err, &pg) || pg.Code != "23505" {
			t.Errorf("a second row of tenant a with its own slug gives %v, want 23505", err)
		}
		// Tenant b cannot tell from its writes whether tenant a holds a slug: a new slug of a holds no key of b.
		if err := insert("b", "00000000-0000-0000-0000-000000000004", "only-a"); err != nil {
			t.Errorf("tenant b: %v", err)
		}
	})

	t.Run("ST-SUP-RP15-LC01-T13 acceptUnique refuses each other unique form on a table with row-level security", func(t *testing.T) {
		const slug = `ALTER TABLE public.books ADD COLUMN "slug" text;` + "\n"
		cases := []struct{ name, setup string }{
			{"no tenant lead", slug + `CREATE UNIQUE INDEX "books_slug_key" ON public.books ("slug");` + "\n"},
			{"the tenant column second", slug + `CREATE UNIQUE INDEX "books_slug_tenant_id_key" ON public.books ("slug", "tenant_id");` + "\n"},
			{"the key id as the lead", slug + `CREATE UNIQUE INDEX "books_slug_key" ON public.books ("id", "slug");` + "\n"},
			{"an expression", slug + `CREATE UNIQUE INDEX "books_slug_key" ON public.books ("tenant_id", lower("slug"));` + "\n"},
			{"an expression on the lead", slug + `CREATE UNIQUE INDEX "books_slug_key" ON public.books (lower("tenant_id"), "slug");` + "\n"},
			{"a predicate", slug + `CREATE UNIQUE INDEX "books_slug_key" ON public.books ("tenant_id", "slug") WHERE "slug" <> '';` + "\n"},
			{"NULLS NOT DISTINCT", slug + `CREATE UNIQUE INDEX "books_slug_key" ON public.books ("tenant_id", "slug") NULLS NOT DISTINCT;` + "\n"},
			{"another name", slug + `CREATE UNIQUE INDEX "books_slug_uq" ON public.books ("tenant_id", "slug");` + "\n"},
			{"a non-default operator class", slug + `CREATE UNIQUE INDEX "books_slug_key" ON public.books ("tenant_id", "slug" text_pattern_ops);` + "\n"},
			{"a non-default collation", slug + `CREATE UNIQUE INDEX "books_slug_key" ON public.books ("tenant_id", "slug" COLLATE "POSIX");` + "\n"},
			{"a deferrable unique constraint", slug + `ALTER TABLE public.books ADD CONSTRAINT "books_slug_key" UNIQUE ("tenant_id", "slug") DEFERRABLE;` + "\n"},
			{"listed columns only", `CREATE UNIQUE INDEX "books_title_key" ON public.books ("tenant_id", "title");` + "\n"},
			{"the lead twice", slug + `CREATE UNIQUE INDEX "books_tenant_id_slug_key" ON public.books ("tenant_id", "tenant_id", "slug");` + "\n"},
			{"a domain column", "CREATE DOMAIN public.dom AS text;\nALTER TABLE public.books ADD COLUMN \"slug\" public.dom;\n" +
				`CREATE UNIQUE INDEX "books_slug_key" ON public.books ("tenant_id", "slug");` + "\n"},
		}
		for _, c := range cases {
			stepCase(t, c.name, c.setup, stepManifest(), true)
		}
		// The partition-only index is in TestStepPartitions.
		stepCase(t, "control: the step form", slug+`CREATE UNIQUE INDEX "books_slug_key" ON public.books ("tenant_id", "slug");`+"\n", stepManifest(), false)
		stepCase(t, "control: the step form as a unique constraint", slug+`ALTER TABLE public.books ADD CONSTRAINT "books_slug_key" UNIQUE ("tenant_id", "slug");`+"\n", stepManifest(), false)
		stepCase(t, "control: the step form with INCLUDE", slug+`CREATE UNIQUE INDEX "books_slug_key" ON public.books ("tenant_id", "slug") INCLUDE ("title");`+"\n", stepManifest(), false)
	})

	// Defence in depth: on an entity table with row-level security whose primary key is the key id alone, the lead
	// is empty, so acceptUnique accepts a unique index with no tenant column. The generator gives no such table
	// (an owned table leads with its tenant column), so this case shows what Check takes from a manifest that is not
	// generated.
	t.Run("ST-SUP-RP15-LC01-T13 a unique index with no tenant lead on an RLS entity table keyed by id alone", func(t *testing.T) {
		m := stepManifest()
		m.Tables[1].Constraints[0].Definition = "PRIMARY KEY (id)"
		m.Tables[1].Indexes[0].Columns = []string{"id"}
		setup := `ALTER TABLE public.books DROP CONSTRAINT books_pkey; ALTER TABLE public.books ADD CONSTRAINT books_pkey PRIMARY KEY (id);
ALTER TABLE public.books ADD COLUMN "slug" text;
CREATE UNIQUE INDEX "books_slug_key" ON public.books ("slug");
`
		admin, reader := database(t)
		faults := formFaults(t, admin, reader, setup, m)
		t.Logf("faults: %q", faults)
		if faults == nil {
			t.Errorf("Check accepts a unique index with no tenant column on a table with row-level security keyed by id alone")
		}
	})
}

func TestStepPartitions(t *testing.T) {
	t.Run("ST-SUP-RP15-LC01-T14 an object of a partition alone is refused", func(t *testing.T) {
		cases := []struct{ name, setup string }{
			{"a foreign key", "ALTER TABLE public.books_all ADD CONSTRAINT books_all_x_fkey FOREIGN KEY (tenant_id, folder_id) REFERENCES public.folders (tenant_id, id);\n"},
			{"a foreign key NOT VALID", "ALTER TABLE public.books_all ADD CONSTRAINT books_all_x_fkey FOREIGN KEY (tenant_id, folder_id) REFERENCES public.folders (tenant_id, id) NOT VALID;\n"},
			{"a foreign key on one column to a table keyed by id", "CREATE TABLE public.shelf (id uuid PRIMARY KEY);\nALTER TABLE public.books_all ADD CONSTRAINT books_all_x_fkey FOREIGN KEY (folder_id) REFERENCES public.shelf (id);\n"},
			{"a CHECK of built-in code", "ALTER TABLE public.books_all ADD CONSTRAINT books_all_x_check CHECK (char_length(title) < 100);\n"},
			{"a CHECK NOT VALID", "ALTER TABLE public.books_all ADD CONSTRAINT books_all_x_check CHECK (public.evil0()) NOT VALID;\n"},
			{"a CHECK NO INHERIT", "ALTER TABLE public.books_all ADD CONSTRAINT books_all_x_check CHECK (public.evil0()) NO INHERIT;\n"},
			{"a unique index", "CREATE UNIQUE INDEX books_all_title_key ON public.books_all (title);\n"},
			{"a unique index with the tenant lead", "CREATE UNIQUE INDEX books_all_title_key ON public.books_all (tenant_id, title);\n"},
			{"a unique constraint", "ALTER TABLE public.books_all ADD CONSTRAINT books_all_title_key UNIQUE (title);\n"},
			{"a unique index with INCLUDE", "CREATE UNIQUE INDEX books_all_title_key ON public.books_all (title) INCLUDE (note);\n"},
			{"an exclusion constraint", "ALTER TABLE public.books_all ADD CONSTRAINT books_all_x_excl EXCLUDE USING btree (title WITH =);\n"},
			{"an expression index", "CREATE INDEX books_all_x ON public.books_all (public.evilt(title));\n"},
			// A table attached as a partition keeps its own objects.
			{"a table attached with its own unique index and CHECK", "CREATE TABLE public.books_b (LIKE public.books INCLUDING CONSTRAINTS);\n" +
				"CREATE UNIQUE INDEX books_b_title_key ON public.books_b (title);\nALTER TABLE public.books_b ADD CONSTRAINT books_b_x_check CHECK (public.evil0());\n" +
				"ALTER TABLE public.books DETACH PARTITION public.books_all;\nDROP TABLE public.books_all;\nALTER TABLE public.books ATTACH PARTITION public.books_b DEFAULT;\n" +
				"CREATE TRIGGER books_b_mark BEFORE TRUNCATE ON public.books_b FOR EACH STATEMENT EXECUTE FUNCTION public.books_reject();\n"},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				admin, reader := database(t)
				faults := formFaults(t, admin, reader, partitionedBooks+hostileFunctions+c.setup, partitionedWant())
				if faults == nil {
					t.Error("Check accepts the object of the partition alone")
				}
				for _, f := range faults {
					if !strings.Contains(f, "of the partition alone") {
						t.Logf("other fault: %s", f)
					}
				}
			})
		}
		// A unique index of the partition cannot be attached to a plain index of the table.
		t.Run("a unique partition index attached to a plain index of the table", func(t *testing.T) {
			admin, _ := database(t)
			if _, err := admin.Exec(partitionedBooks + "CREATE INDEX books_note ON ONLY public.books (note);\nCREATE UNIQUE INDEX books_all_note ON public.books_all (note);\n"); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.Exec("ALTER INDEX public.books_note ATTACH PARTITION public.books_all_note"); err == nil {
				t.Error("PostgreSQL attaches a unique partition index to a plain index of the table")
			}
		})
		// Controls.
		t.Run("control: the partitioned table of the manifest", func(t *testing.T) {
			admin, reader := database(t)
			if faults := formFaults(t, admin, reader, partitionedBooks, partitionedWant()); faults != nil {
				t.Errorf("Check refuses: %q", faults)
			}
		})
		t.Run("control: a plain index of the partition alone", func(t *testing.T) {
			admin, reader := database(t)
			if faults := formFaults(t, admin, reader, partitionedBooks+"CREATE INDEX books_all_note ON public.books_all (note);\n", partitionedWant()); faults != nil {
				t.Errorf("Check refuses: %q", faults)
			}
		})
	})
}

// pgDSN gives the DSN of the database of admin as its superuser, from the current database name.
func pgDSN(t *testing.T, admin *sql.DB) string {
	t.Helper()
	var name string
	if err := admin.QueryRow("SELECT current_database()").Scan(&name); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(pgtest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}

// formSync is a test database with a table v (tenant, id, k K-type DEFAULT kDefault, u U-type) owned by a
// non-superuser owner role, row-level security forced, and a sync trigger of the change "v-k" made by SyncFunction as
