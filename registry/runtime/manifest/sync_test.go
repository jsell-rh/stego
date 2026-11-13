package manifest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// stepManifest is the manifest of the test schema with books as the table of a declared entity and stego_steps as
// the held schema of the sync functions.
func stepManifest() Manifest {
	m := want()
	m.Tables[0].Entity = true
	m.Tables[1].Entity = true
	m.Schemas = []Schema{{Name: "stego_steps", Steps: true}}
	return m
}

// syncSQL gives the CREATE FUNCTION of the sync function stego_steps.<function> for k, u and the conversion named
// conversion, of the change whose ID is the function name in kebab case, with testOwner as the backfill role, and
// fails the test when the template cannot render it.
func syncSQL(t *testing.T, function, k, u, conversion string) string {
	t.Helper()
	return syncFillSQL(t, function, k, u, conversion, "", false)
}

// syncFillSQL is syncSQL with a fill value when hasFill.
func syncFillSQL(t *testing.T, function, k, u, conversion, fill string, hasFill bool) string {
	t.Helper()
	c, ok := SyncConversion(conversion)
	if !ok {
		t.Fatalf("no conversion %s", conversion)
	}
	if testOwner == "" {
		database(t)
	}
	statement, err := SyncFunction("stego_steps", function, 1, Sync{Change: strings.ReplaceAll(function, "_", "-"), Owner: testOwner,
		K: k, U: u, Conversion: c, Fill: fill, HasFill: hasFill})
	if err != nil {
		t.Fatal(err)
	}
	return statement + ";\n"
}

// stepCase runs setup on a new test database as its owner and checks it against m: refused tells that Check must
// give a *Mismatch, and else it must accept the database. Each case is a subtest of its own, so its database is
// dropped when the case ends, and the server of the test, whose data is in its 512 MiB of memory, holds few at once.
func stepCase(t *testing.T, name, setup string, m Manifest, refused bool) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		admin, reader := database(t)
		if _, err := admin.Exec(setup); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		m.Owner = testOwner
		err := Check(context.Background(), reader, m)
		var mismatch *Mismatch
		switch {
		case refused && !errors.As(err, &mismatch):
			t.Errorf("%s: Check gives %v, want a refusal", name, err)
		case !refused && err != nil:
			t.Errorf("%s: Check refuses the step form: %v", name, err)
		}
	})
}

const (
	stepSchema = "CREATE SCHEMA stego_steps; REVOKE ALL ON SCHEMA stego_steps FROM PUBLIC;\n"
	noteV2     = "ALTER TABLE public.books ADD COLUMN note_v2 text;\n"
	noteSync   = "CREATE TRIGGER books_note_sync BEFORE INSERT OR UPDATE ON public.books FOR EACH ROW EXECUTE FUNCTION stego_steps.books_note_sync();\n"
)

func TestSyncTrigger(t *testing.T) {
	note := func(t *testing.T) string {
		return stepSchema + noteV2 + syncSQL(t, "books_note_sync", "note", "note_v2", "identity")
	}

	t.Run("ST-SUP-RP15-LC02-T01 Check accepts the sync trigger of a later step in its strict template form", func(t *testing.T) {
		stepCase(t, "identity of note to note_v2", note(t)+noteSync, stepManifest(), false)
		stepCase(t, "the same with the column list {note, note_v2} and enabled always",
			note(t)+"CREATE TRIGGER books_note_sync BEFORE INSERT OR UPDATE OF note, note_v2 ON public.books FOR EACH ROW EXECUTE FUNCTION stego_steps.books_note_sync();\nALTER TABLE public.books ENABLE ALWAYS TRIGGER books_note_sync;\n",
			stepManifest(), false)
		stepCase(t, "integer pages to text pages_text", stepSchema+"ALTER TABLE public.books ADD COLUMN pages_text text;\n"+syncSQL(t, "books_pages_sync", "pages", "pages_text", "int4-text")+
			"CREATE TRIGGER books_pages_sync BEFORE INSERT OR UPDATE ON public.books FOR EACH ROW EXECUTE FUNCTION stego_steps.books_pages_sync();\n", stepManifest(), false)
		stepCase(t, "no step yet: the held schema of the sync functions is missing", "", stepManifest(), false)
		noteInt := stepSchema + "ALTER TABLE public.books ADD COLUMN note_int integer;\n" + syncFillSQL(t, "books_note_sync", "note", "note_int", "text-int4", "0", true) +
			"CREATE TRIGGER books_note_sync BEFORE INSERT OR UPDATE ON public.books FOR EACH ROW EXECUTE FUNCTION stego_steps.books_note_sync();\n"
		stepCase(t, "text note to integer note_int with the fill value 0", noteInt, stepManifest(), false)
		stepCase(t, "text note to integer note_int with no fill value", strings.Replace(noteInt, "ELSE '0' END)", "ELSE NULL END)", -1), stepManifest(), false)
		// Each conversion of the list takes a value of the release before that does not convert to NULL, and never
		// raises (ST-SUP-RC01-OC33).
		t.Run("each conversion never raises for a write of the release before", func(t *testing.T) {
			admin, _ := database(t)
			hostile := map[string]string{"text": "'abc'", "bigint": "9999999999", "integer": "7", "numeric": "1.5", "boolean": "true",
				"uuid": "'00000000-0000-0000-0000-000000000009'"}
			for i, c := range SyncConversions[1:] {
				table := fmt.Sprintf("conv_%d", i)
				statement, err := SyncFunction("public", table+"_sync", 1, Sync{Change: "conv", Owner: testOwner, K: "k", U: "u", Conversion: c})
				if err != nil {
					t.Fatal(err)
				}
				mustExec(t, admin, fmt.Sprintf("CREATE TABLE public.%[1]s (k %[2]s, u %[3]s);\n%[4]s;\nCREATE TRIGGER s BEFORE INSERT OR UPDATE ON public.%[1]s FOR EACH ROW EXECUTE FUNCTION public.%[1]s_sync();", table, c.From, c.To, statement))
				if _, err := admin.Exec(fmt.Sprintf("INSERT INTO public.%s (k) VALUES (%s)", table, hostile[c.From])); err != nil {
					t.Errorf("the conversion %s raises for a write of the release before: %v", c.Name, err)
				}
			}
		})
		// A value of the release before that does not convert gets the fill value, and the write is not refused
		// (ST-SUP-RC01-OC33).
		t.Run("a value that does not convert", func(t *testing.T) {
			admin, _ := database(t)
			mustExec(t, admin, noteInt+`DROP TRIGGER books_reject_row ON public.books; SET app.tenant = 't';
INSERT INTO public.folders VALUES ('t', '00000000-0000-0000-0000-000000000001');`)
			for _, c := range []struct {
				note string
				want int64
			}{{"12", 12}, {"abc", 0}, {"99999999999", 0}, {"", -1}} {
				if c.want < 0 {
					// A NULL of the release before stays NULL, and never takes the fill value.
					mustExec(t, admin, `SET app.tenant = 't'; DELETE FROM public.books; INSERT INTO public.books (id, folder_id, title) VALUES (gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'a')`)
					var got sql.NullInt64
					if err := admin.QueryRow(`SELECT note_int FROM public.books`).Scan(&got); err != nil || got.Valid {
						t.Errorf("an INSERT of the release before with no note gives note_int %v (%v), want NULL", got, err)
					}
					continue
				}
				mustExec(t, admin, `SET app.tenant = 't'; DELETE FROM public.books; INSERT INTO public.books (id, folder_id, title, note) VALUES (gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'a', '`+c.note+`')`)
				var got int64
				if err := admin.QueryRow(`SELECT note_int FROM public.books`).Scan(&got); err != nil || got != c.want {
					t.Errorf("an INSERT of the release before of %q gives note_int %d (%v), want %d", c.note, got, err, c.want)
				}
			}
		})
		// The trigger moves the rows as rule 3 says: the writer marker picks the direction. The release before
		// writes only K and sets no marker; the release of the step writes only U and names the change in the marker;
		// a write that sets both to values that differ is refused (ST-SUP-RC01-OC23). note has a declared default,
		// which K takes on an INSERT of the release of the step (decision on finding F3).
		syncWrites(t, note(t)+noteSync)
	})

	t.Run("ST-SUP-RP15-LC02-T02 Check refuses a trigger that breaks any rule of the strict template form", func(t *testing.T) {
		base := note(t)
		plain := stepManifest()
		plain.Tables[1].Entity = false
		noSteps := stepManifest()
		noSteps.Schemas = nil
		edited := strings.Replace(base, "NEW.note := NEW.note_v2;", "NEW.note_v2 := NEW.note;", 1)
		for _, c := range []struct{ name, setup string }{
			// Rule 2: the pair.
			{"K is the key id", stepSchema + "ALTER TABLE public.books ADD COLUMN id_v2 uuid;\n" + syncSQL(t, "books_id_sync", "id", "id_v2", "identity") +
				"CREATE TRIGGER books_id_sync BEFORE INSERT OR UPDATE ON public.books FOR EACH ROW EXECUTE FUNCTION stego_steps.books_id_sync();\n"},
			{"K is the tenant column", stepSchema + "ALTER TABLE public.books ADD COLUMN tenant_v2 text;\n" + syncSQL(t, "books_tenant_sync", "tenant_id", "tenant_v2", "identity") +
				"CREATE TRIGGER books_tenant_sync BEFORE INSERT OR UPDATE ON public.books FOR EACH ROW EXECUTE FUNCTION stego_steps.books_tenant_sync();\n"},
			{"K is in a unique key", stepSchema + "ALTER TABLE public.books ADD COLUMN folder_v2 uuid;\n" + syncSQL(t, "books_folder_sync", "folder_id", "folder_v2", "identity") +
				"CREATE TRIGGER books_folder_sync BEFORE INSERT OR UPDATE ON public.books FOR EACH ROW EXECUTE FUNCTION stego_steps.books_folder_sync();\n"},
			{"K is not in the manifest", stepSchema + "ALTER TABLE public.books ADD COLUMN extra text, ADD COLUMN extra_v2 text;\n" + syncSQL(t, "books_extra_sync", "extra", "extra_v2", "identity") +
				"CREATE TRIGGER books_extra_sync BEFORE INSERT OR UPDATE ON public.books FOR EACH ROW EXECUTE FUNCTION stego_steps.books_extra_sync();\n"},
			{"U has a default", strings.Replace(base, "ADD COLUMN note_v2 text", "ADD COLUMN note_v2 text DEFAULT 'x'", 1) + noteSync},
			{"U is generated", strings.Replace(base, "ADD COLUMN note_v2 text", "ADD COLUMN note_v2 text GENERATED ALWAYS AS (upper(note)) STORED", 1) + noteSync},
			{"the conversion does not fit the types", stepSchema + "ALTER TABLE public.books ADD COLUMN note_v2 integer;\n" + syncSQL(t, "books_note_sync", "note", "note_v2", "identity") + noteSync},
			{"two sync triggers", base + noteSync + "ALTER TABLE public.books ADD COLUMN pages_text text;\n" + syncSQL(t, "books_pages_sync", "pages", "pages_text", "int4-text") +
				"CREATE TRIGGER books_pages_sync BEFORE INSERT OR UPDATE ON public.books FOR EACH ROW EXECUTE FUNCTION stego_steps.books_pages_sync();\n"},
			// Rules 3, 4 and 6: the function.
			{"the body moves the other way", edited + noteSync},
			{"the body has one more space", strings.Replace(base, "RETURN NEW;", "RETURN NEW; ", 1) + noteSync},
			{"a conversion that is not in the list", strings.Replace(base, "$stego_sync$\nBEGIN", "$stego_sync$\nBEGIN", 1) + noteSync + "CREATE OR REPLACE FUNCTION stego_steps.books_note_sync() RETURNS trigger LANGUAGE plpgsql SECURITY INVOKER SET search_path = pg_catalog, pg_temp AS $f$" +
				strings.ReplaceAll(mustSource(t, "note", "note_v2"), "NEW.note;", "pg_catalog.lower(NEW.note);") + "$f$;\n"},
			{"SECURITY DEFINER", base + noteSync + "ALTER FUNCTION stego_steps.books_note_sync() SECURITY DEFINER;\n"},
			{"another setting", base + noteSync + "ALTER FUNCTION stego_steps.books_note_sync() SET work_mem = '64kB';\n"},
			{"no search path", base + noteSync + "ALTER FUNCTION stego_steps.books_note_sync() RESET search_path;\n"},
			{"STRICT", base + noteSync + "ALTER FUNCTION stego_steps.books_note_sync() STRICT;\n"},
			{"another owner", base + noteSync + "DO $$ BEGIN CREATE ROLE sync_other_owner; EXCEPTION WHEN duplicate_object THEN NULL; END $$; ALTER FUNCTION stego_steps.books_note_sync() OWNER TO sync_other_owner;\n"},
			{"in a schema that is not the held schema of the sync functions", strings.ReplaceAll(base+noteSync, "stego_steps.", "public.")},
			{"bound to two triggers", base + noteSync + "CREATE TABLE public.other (note text, note_v2 text);\nCREATE TRIGGER other_sync BEFORE INSERT OR UPDATE ON public.other FOR EACH ROW EXECUTE FUNCTION stego_steps.books_note_sync();\n"},
			// Rule 5: the trigger.
			{"AFTER", base + strings.Replace(noteSync, "BEFORE", "AFTER", 1)},
			{"FOR EACH STATEMENT", base + strings.Replace(noteSync, "FOR EACH ROW", "FOR EACH STATEMENT", 1)},
			{"INSERT only", base + strings.Replace(noteSync, "INSERT OR UPDATE", "INSERT", 1)},
			{"also on DELETE", base + strings.Replace(noteSync, "INSERT OR UPDATE", "INSERT OR UPDATE OR DELETE", 1)},
			{"a WHEN condition", base + strings.Replace(noteSync, "FOR EACH ROW", "FOR EACH ROW WHEN (NEW.note IS NOT NULL)", 1)},
			{"another column list", base + strings.Replace(noteSync, "INSERT OR UPDATE", "INSERT OR UPDATE OF note", 1)},
			{"disabled", base + noteSync + "ALTER TABLE public.books DISABLE TRIGGER books_note_sync;\n"},
			{"enabled for a replica only", base + noteSync + "ALTER TABLE public.books ENABLE REPLICA TRIGGER books_note_sync;\n"},
			// U is of a type that is not built in: a domain named public.text, which the template's text cannot tell
			// from pg_catalog.text, and whose CHECK would run on each INSERT.
			{"U is a domain named public.text", stepSchema + "CREATE DOMAIN public.text AS pg_catalog.text CHECK (VALUE <> 'x');\nALTER TABLE public.books ADD COLUMN pages_text public.text;\n" +
				syncSQL(t, "books_pages_sync", "pages", "pages_text", "int4-text") +
				"CREATE TRIGGER books_pages_sync BEFORE INSERT OR UPDATE ON public.books FOR EACH ROW EXECUTE FUNCTION stego_steps.books_pages_sync();\n"},
			// The held schema of the sync functions holds no other function.
			{"another function in the held schema", base + noteSync + "CREATE FUNCTION stego_steps.helper() RETURNS integer LANGUAGE sql AS $$ SELECT 1 $$;\n"},
		} {
			stepCase(t, c.name, c.setup, stepManifest(), true)
		}
		// Rule 1: the scope, and the held schema.
		stepCase(t, "a table that is not the table of a declared entity", base+noteSync, plain, true)
		stepCase(t, "a manifest with no held schema of the sync functions", base+noteSync, noSteps, true)
		// SyncSource renders no constant with a backslash or a control character, and no fill value for a conversion
		// that cannot fail.
		for name, c := range map[string]Sync{
			"a fill value with a backslash":      {Change: "c", Owner: "o", K: "k", U: "u", Conversion: SyncConversions[len(SyncConversions)-1], Fill: `a\`, HasFill: true},
			"a backfill role with a backslash":   {Change: "c", Owner: `o\`, K: "k", U: "u", Conversion: SyncConversions[0]},
			"a fill value with a line break":     {Change: "c", Owner: "o", K: "k", U: "u", Conversion: SyncConversions[len(SyncConversions)-1], Fill: "a\n", HasFill: true},
			"a fill value on the identity":       {Change: "c", Owner: "o", K: "k", U: "u", Conversion: SyncConversions[0], Fill: "a", HasFill: true},
			"a change ID that is not kebab case": {Change: "C_1", Owner: "o", K: "k", U: "u", Conversion: SyncConversions[0]},
			"no backfill role":                   {Change: "c", K: "k", U: "u", Conversion: SyncConversions[0]},
		} {
			if _, err := SyncSource(1, c); err == nil {
				t.Errorf("SyncSource renders %s", name)
			}
		}
		// Rule 6: a version that this release does not build is no template.
		if _, err := SyncSource(2, Sync{Change: "books-note-sync", Owner: testOwner, K: "note", U: "note_v2", Conversion: SyncConversions[0]}); err == nil {
			t.Error("SyncSource renders version 2, which this release does not build")
		}
		// A manifest that marks a runtime or a partitioned table as an entity, or holds two schemas of the sync
		// functions, or a shared one, is refused.
		for name, m := range map[string]Manifest{
			"runtime table": {Tables: []Table{{Schema: "stego_outbox", Name: "messages", Entity: true}}},
			"partitioned":   {Tables: []Table{{Schema: "public", Name: "events", Entity: true, Partitioned: true}}},
			"two schemas":   {Owner: "o", Schemas: []Schema{{Name: "a", Steps: true}, {Name: "b", Steps: true}}},
			"shared":        {Owner: "o", Schemas: []Schema{{Name: "public", Shared: true, Steps: true}}},
		} {
			if err := m.Validate(); err == nil {
				t.Errorf("Validate accepts a manifest with a %s", name)
			}
		}
	})

	t.Run("ST-SUP-RP15-LC02-T03 An optional trigger may be missing, and is exact when present, never by a prefix", func(t *testing.T) {
		source := mustSource(t, "note", "note_v2")
		optional := stepManifest()
		optional.Tables[1].Columns = append(optional.Tables[1].Columns, Column{Name: "note_v2", Type: "text"})
		optional.Tables[1].OptionalTriggers = []Trigger{{Name: "books_note_sync", Timing: "BEFORE", Row: true, Events: []string{"INSERT", "UPDATE"},
			Function: Function{Name: "stego_steps.books_note_sync", Language: "plpgsql", Source: source, Config: []string{SyncConfig}}}}
		base := note(t)
		stepCase(t, "present and exact", base+noteSync, optional, false)
		stepCase(t, "missing, after the contract step", stepSchema+noteV2, optional, false)
		stepCase(t, "present with another body", strings.Replace(base, "RETURN NEW;", "RETURN NEW; ", 1)+noteSync, optional, true)
		stepCase(t, "present and disabled", base+noteSync+"ALTER TABLE public.books DISABLE TRIGGER books_note_sync;\n", optional, true)
		stepCase(t, "another sync trigger on a column of the optional one", base+noteSync+"ALTER TABLE public.books ADD COLUMN note_v3 text;\n"+
			syncSQL(t, "books_note3_sync", "note", "note_v3", "identity")+
			"CREATE TRIGGER books_note3_sync BEFORE INSERT OR UPDATE ON public.books FOR EACH ROW EXECUTE FUNCTION stego_steps.books_note3_sync();\n", optional, true)
		stepCase(t, "a trigger whose name has the optional name as a prefix", stepSchema+noteV2+
			"CREATE FUNCTION public.books_other() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$;\n"+
			"CREATE TRIGGER books_note_sync_2 BEFORE INSERT ON public.books FOR EACH ROW EXECUTE FUNCTION public.books_other();\n", optional, true)
	})
}

// mustSource gives the source of version 1 of the sync template for k and u with the identity, of the change
// books-note-sync with testOwner as the backfill role.
func mustSource(t *testing.T, k, u string) string {
	t.Helper()
	if testOwner == "" {
		database(t)
	}
	source, err := SyncSource(1, Sync{Change: "books-note-sync", Owner: testOwner, K: k, U: u, Conversion: SyncConversions[0]})
	if err != nil {
		t.Fatal(err)
	}
	return source
}

// syncWrites runs the writes of rule 3 on a new test database made by setup, which holds the sync trigger of the change
// books-note-sync from note to note_v2 with the identity, and checks both columns after each one.
func syncWrites(t *testing.T, setup string) {
	t.Helper()
	admin, _ := database(t)
	mustExec(t, admin, setup+`DROP TRIGGER books_reject_row ON public.books; ALTER TABLE public.books ALTER COLUMN note SET DEFAULT 'dflt';
SET app.tenant = 't'; INSERT INTO public.folders VALUES ('t', '00000000-0000-0000-0000-000000000001');`)
	const before, step = "", "SET LOCAL stego.schema_step = 'other-change,books-note-sync'; "
	id := func(n string) string { return "'00000000-0000-0000-0000-00000000000" + n + "'" }
	insert := func(marker, n, columns, values string) error {
		_, err := admin.Exec(marker + `SET LOCAL app.tenant = 't'; INSERT INTO public.books (id, folder_id, title` + columns + `) VALUES (` + id(n) +
			`, '00000000-0000-0000-0000-000000000001', 'a'` + values + `)`)
		return err
	}
	update := func(marker, n, set string) error {
		_, err := admin.Exec(marker + `SET LOCAL app.tenant = 't'; UPDATE public.books SET ` + set + ` WHERE id = ` + id(n))
		return err
	}
	want := func(what, n string, note, v2 sql.NullString) {
		t.Helper()
		var gn, gv sql.NullString
		if err := admin.QueryRow(`SELECT note, note_v2 FROM public.books WHERE id = `+id(n)).Scan(&gn, &gv); err != nil {
			t.Fatal(err)
		}
		if gn != note || gv != v2 {
			t.Errorf("after %s note and note_v2 are %v and %v, want %v and %v", what, gn, gv, note, v2)
		}
	}
	text := func(v string) sql.NullString { return sql.NullString{String: v, Valid: true} }
	null := sql.NullString{}
	refused := func(what string, err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), "23514") && !strings.Contains(err.Error(), "values that differ") {
			t.Errorf("%s gives %v, want the refusal of the sync trigger", what, err)
		}
		if err != nil && (strings.Contains(err.Error(), "'x'") || strings.Contains(err.Error(), "'y'")) {
			t.Errorf("the refusal holds a row value: %v", err)
		}
	}
	accepted := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Errorf("%s: %v", what, err)
		}
	}
	// The release before: an INSERT of K sets U; with no value, K takes its default and U the same.
	accepted("an INSERT of the release before", insert(before, "2", ", note", ", 'old'"))
	want("an INSERT of the release before", "2", text("old"), text("old"))
	accepted("an INSERT of the release before with no note", insert(before, "5", "", ""))
	want("an INSERT of the release before with no note", "5", text("dflt"), text("dflt"))
	refused("an INSERT of the release before that sets U to another value", insert(before, "6", ", note, note_v2", ", 'x', 'y'"))
	accepted("an INSERT of the release before that sets U to the same value", insert(before, "6", ", note, note_v2", ", 'same', 'same'"))
	// The release of the step: an INSERT of U sets K, over the default of K, and an explicit NULL stays NULL.
	accepted("an INSERT of the release of the step", insert(step, "3", ", note_v2", ", 'new'"))
	want("an INSERT of the release of the step", "3", text("new"), text("new"))
	accepted("an INSERT of the release of the step with a NULL", insert(step, "4", ", note_v2", ", NULL"))
	want("an INSERT of the release of the step with a NULL", "4", null, null)
	// The marker names the change only as a whole item of its list.
	accepted("an INSERT under a marker of another change", insert("SET LOCAL stego.schema_step = 'books-note-sync-2,books-note'; ", "7", ", note", ", 'k'"))
	want("an INSERT under a marker of another change", "7", text("k"), text("k"))
	// UPDATE: the column that changed is copied into the other, by either release.
	accepted("an UPDATE of U by the release of the step", update(step, "2", "note_v2 = 'new'"))
	want("an UPDATE of U by the release of the step", "2", text("new"), text("new"))
	accepted("an UPDATE of K by the release before", update(before, "2", "note = 'k'"))
	want("an UPDATE of K by the release before", "2", text("k"), text("k"))
	accepted("an UPDATE of U with no marker", update(before, "2", "note_v2 = 'u'"))
	want("an UPDATE of U with no marker, which takes U from K", "2", text("k"), text("k"))
	accepted("an UPDATE of a third column", update(step, "2", "pages = 3"))
	want("an UPDATE of a third column", "2", text("k"), text("k"))
	refused("an UPDATE of both with values that differ by the release before", update(before, "2", "note = 'x', note_v2 = 'y'"))
	refused("an UPDATE of both with values that differ by the release of the step", update(step, "2", "note = 'x', note_v2 = 'y'"))
	want("the refused UPDATEs", "2", text("k"), text("k"))
	accepted("an UPDATE of both with one value", update(step, "2", "note = 'z', note_v2 = 'z'"))
	want("an UPDATE of both with one value", "2", text("z"), text("z"))
	// The backfill bypass: only the backfill role, the owner of the manifest, passes the trigger of its own change.
	accepted("an UPDATE of the backfill", update("SET LOCAL stego.backfill_change = 'books-note-sync'; ", "2", "note_v2 = 'b'"))
	want("an UPDATE of the backfill", "2", text("z"), text("b"))
	accepted("an UPDATE of the backfill of another change", update("SET LOCAL stego.backfill_change = 'other'; ", "3", "note_v2 = 'b'"))
	want("an UPDATE of the backfill of another change", "3", text("new"), text("new"))
	mustExec(t, admin, `DO $$ BEGIN CREATE ROLE sync_other_writer; EXCEPTION WHEN duplicate_object THEN NULL; END $$;
GRANT USAGE ON SCHEMA stego_steps TO sync_other_writer; GRANT SELECT, UPDATE ON public.books TO sync_other_writer;`)
	accepted("an UPDATE of the backfill setting by another role", update("SET LOCAL ROLE sync_other_writer; SET LOCAL stego.backfill_change = 'books-note-sync'; ", "3", "note_v2 = 'c'"))
	want("an UPDATE of the backfill setting by another role", "3", text("new"), text("new"))
}

func mustExec(t *testing.T, db *sql.DB, statement string) {
	t.Helper()
	if _, err := db.Exec(statement); err != nil {
		t.Fatal(err)
	}
}

func TestStepForeignKey(t *testing.T) {
	const shelf = "ALTER TABLE public.books ADD COLUMN shelf_id uuid;\n"
	const fk = "ALTER TABLE public.books ADD CONSTRAINT books_shelf_id_fkey FOREIGN KEY (tenant_id, shelf_id) REFERENCES public.folders (tenant_id, id);\n"
	t.Run("ST-SUP-RP15-LC03-T01 Check accepts the foreign key of a later add step only in its exact form", func(t *testing.T) {
		stepCase(t, "the exact form", shelf+fk, stepManifest(), false)
		plain := stepManifest()
		plain.Tables[1].Entity = false
		stepCase(t, "a table that is not the table of a declared entity", shelf+fk, plain, true)
		listed := stepManifest()
		listed.Tables[1].Columns = append(listed.Tables[1].Columns, Column{Name: "shelf_id", Type: "uuid"})
		stepCase(t, "a column of the manifest", shelf+fk, listed, true)
		for _, c := range []struct{ name, setup string }{
			{"a NOT NULL column", strings.Replace(shelf, "uuid", "uuid NOT NULL", 1) + fk},
			{"ON DELETE CASCADE", shelf + strings.Replace(fk, ";", " ON DELETE CASCADE;", 1)},
			{"ON DELETE SET NULL", shelf + strings.Replace(fk, ";", " ON DELETE SET NULL (shelf_id);", 1)},
			{"ON UPDATE RESTRICT", shelf + strings.Replace(fk, ";", " ON UPDATE RESTRICT;", 1)},
			{"MATCH FULL", shelf + strings.Replace(fk, ";", " MATCH FULL;", 1)},
			{"DEFERRABLE", shelf + strings.Replace(fk, ";", " DEFERRABLE;", 1)},
			{"NOT VALID", shelf + strings.Replace(fk, ";", " NOT VALID;", 1)},
			{"another name", shelf + strings.Replace(fk, "books_shelf_id_fkey", "books_shelf_fk", 1)},
			{"without the tenant column", shelf + "CREATE UNIQUE INDEX folders_id_key ON public.folders (id);\nALTER TABLE public.books ADD CONSTRAINT books_shelf_id_fkey FOREIGN KEY (shelf_id) REFERENCES public.folders (id);\n"},
			{"the tenant column second", shelf + "CREATE UNIQUE INDEX folders_id_tenant_key ON public.folders (id, tenant_id);\nALTER TABLE public.books ADD CONSTRAINT books_shelf_id_fkey FOREIGN KEY (shelf_id, tenant_id) REFERENCES public.folders (id, tenant_id);\n"},
			{"a unique key that is not the primary key", shelf + "ALTER TABLE public.folders ADD COLUMN alt uuid;\nCREATE UNIQUE INDEX folders_alt_key ON public.folders (tenant_id, alt);\nALTER TABLE public.books ADD CONSTRAINT books_shelf_id_fkey FOREIGN KEY (tenant_id, shelf_id) REFERENCES public.folders (tenant_id, alt);\n"},
			{"a table that is not in the manifest", shelf + "CREATE TABLE public.shelves (tenant_id text, id uuid, PRIMARY KEY (tenant_id, id));\nALTER TABLE public.books ADD CONSTRAINT books_shelf_id_fkey FOREIGN KEY (tenant_id, shelf_id) REFERENCES public.shelves (tenant_id, id);\n"},
			{"two new columns", shelf + "ALTER TABLE public.books ADD COLUMN shelf_tenant text;\nALTER TABLE public.books ADD CONSTRAINT books_shelf_id_fkey FOREIGN KEY (shelf_tenant, shelf_id) REFERENCES public.folders (tenant_id, id);\n"},
			{"a column of a domain", "CREATE DOMAIN public.ref AS uuid CHECK (VALUE IS NOT NULL OR VALUE IS NULL);\nALTER TABLE public.books ADD COLUMN shelf_id public.ref;\n" + fk},
		} {
			stepCase(t, c.name, c.setup, stepManifest(), true)
		}
		// A table of the manifest that is not the table of a declared entity is no target.
		folders := stepManifest()
		folders.Tables[0].Entity = false
		stepCase(t, "a referenced table that is not the table of a declared entity", shelf+fk, folders, true)
		// A key that is the key id alone: any table may refer to it with one column when it is the table of a declared
		// entity that is not owned, whose rows each tenant may read; never a runtime table or a table with row-level
		// security (review of PR #270, M1: else the check of the key, which runs past row-level security, tells a
		// tenant whether a row of another tenant exists).
		messages := Table{Schema: "stego_outbox", Name: "messages",
			Columns:     []Column{{Name: "tenant_id", Type: "text", NotNull: true}, {Name: "id", Type: "uuid", NotNull: true}},
			Constraints: []Constraint{{Name: "messages_pkey", Kind: KindPrimaryKey, Definition: "PRIMARY KEY (id)"}},
			Indexes:     []Index{{Name: "messages_pkey", Columns: []string{"id"}}},
			RowSecurity: true, Policies: []Policy{{Name: "t", Command: "*", Using: tenantWall, WithCheck: tenantWall}}}
		const messagesSQL = `CREATE SCHEMA stego_outbox; GRANT USAGE ON SCHEMA stego_outbox TO PUBLIC;
CREATE TABLE stego_outbox.messages (tenant_id text NOT NULL, id uuid NOT NULL, CONSTRAINT messages_pkey PRIMARY KEY (id));
ALTER TABLE stego_outbox.messages ENABLE ROW LEVEL SECURITY; ALTER TABLE stego_outbox.messages FORCE ROW LEVEL SECURITY;
CREATE POLICY t ON stego_outbox.messages USING (tenant_id = pg_catalog.current_setting('app.tenant', true)) WITH CHECK (tenant_id = pg_catalog.current_setting('app.tenant', true));
`
		msgs := messages
		msgs.Schema, msgs.Name, msgs.Entity = "public", "msgs", true
		msgs.Constraints, msgs.Indexes = []Constraint{{Name: "msgs_pkey", Kind: KindPrimaryKey, Definition: "PRIMARY KEY (id)"}},
			[]Index{{Name: "msgs_pkey", Columns: []string{"id"}}}
		const msgsSQL = `CREATE TABLE public.msgs (tenant_id text NOT NULL, id uuid NOT NULL, CONSTRAINT msgs_pkey PRIMARY KEY (id));
ALTER TABLE public.msgs ENABLE ROW LEVEL SECURITY; ALTER TABLE public.msgs FORCE ROW LEVEL SECURITY;
CREATE POLICY t ON public.msgs USING (tenant_id = pg_catalog.current_setting('app.tenant', true)) WITH CHECK (tenant_id = pg_catalog.current_setting('app.tenant', true));
`
		tags := Table{Schema: "public", Name: "tags", Entity: true,
			Columns:     []Column{{Name: "id", Type: "uuid", NotNull: true}},
			Constraints: []Constraint{{Name: "tags_pkey", Kind: KindPrimaryKey, Definition: "PRIMARY KEY (id)"}},
			Indexes:     []Index{{Name: "tags_pkey", Columns: []string{"id"}}}}
		labels := tags
		labels.Name, labels.CheckGrants = "labels", true
		labels.Constraints, labels.Indexes = []Constraint{{Name: "labels_pkey", Kind: KindPrimaryKey, Definition: "PRIMARY KEY (id)"}},
			[]Index{{Name: "labels_pkey", Columns: []string{"id"}}}
		m := stepManifest()
		m.Tables = append(m.Tables, messages, msgs, tags, labels)
		const tablesSQL = messagesSQL + msgsSQL + "CREATE TABLE public.tags (id uuid NOT NULL, CONSTRAINT tags_pkey PRIMARY KEY (id));\n" +
			"CREATE TABLE public.labels (id uuid NOT NULL, CONSTRAINT labels_pkey PRIMARY KEY (id));\n" +
			"ALTER TABLE public.books ADD COLUMN ref_id uuid;\nALTER TABLE public.labels ADD COLUMN ref_id uuid;\n"
		for _, c := range []struct {
			name, table, target string
			accepted            bool
		}{
			{"an owned table to a shared entity table", "books", "public.tags", true},
			{"a shared table to a shared entity table", "labels", "public.tags", true},
			{"an owned table to a runtime table (probe C1)", "books", "stego_outbox.messages", false},
			{"a shared table to a runtime table (probe C1)", "labels", "stego_outbox.messages", false},
			{"an owned table to an entity table with row-level security keyed by id alone", "books", "public.msgs", false},
			{"a shared table to an entity table with row-level security keyed by id alone", "labels", "public.msgs", false},
		} {
			stepCase(t, c.name, tablesSQL+"ALTER TABLE public."+c.table+" ADD CONSTRAINT "+c.table+"_ref_id_fkey FOREIGN KEY (ref_id) REFERENCES "+c.target+" (id);\n", m, !c.accepted)
		}
		stepCase(t, "a shared entity table with row-level security that the manifest does not state", tablesSQL+"ALTER TABLE public.tags ENABLE ROW LEVEL SECURITY;\n"+
			"ALTER TABLE public.books ADD CONSTRAINT books_ref_id_fkey FOREIGN KEY (ref_id) REFERENCES public.tags (id);\n", m, true)
		stepCase(t, "one column to an owned key through a unique index", tablesSQL+
			"CREATE UNIQUE INDEX folders_id_key ON public.folders (id);\nALTER TABLE public.labels ADD CONSTRAINT labels_ref_id_fkey FOREIGN KEY (ref_id) REFERENCES public.folders (id);\n", m, true)
		if got := ForeignKeyName(strings.Repeat("t", 40), strings.Repeat("c", 30)); len(got) != 63 || !strings.HasPrefix(got, strings.Repeat("t", 40)+"_ccccc_") {
			t.Errorf("ForeignKeyName gives %q for a long name", got)
		}
	})
}

func TestStepColumnType(t *testing.T) {
	t.Run("ST-SUP-RP15-LC01-T10 Check refuses a column of a table that the manifest does not list and whose type is not built in", func(t *testing.T) {
		for _, c := range []struct{ name, setup string }{
			{"a text column", "ALTER TABLE public.books ADD COLUMN label text;\n"},
			{"a uuid column", "ALTER TABLE public.books ADD COLUMN shelf_id uuid;\n"},
			{"an array of a built-in type", "ALTER TABLE public.books ADD COLUMN tags text[];\n"},
		} {
			stepCase(t, c.name, c.setup, stepManifest(), false)
		}
		const hostile = "CREATE FUNCTION public.ran(anyelement) RETURNS boolean LANGUAGE plpgsql AS $f$ BEGIN RAISE EXCEPTION 'ran'; END $f$;\n"
		for _, c := range []struct{ name, setup string }{
			{"a domain whose CHECK calls a function", hostile + "CREATE DOMAIN public.dom AS text CHECK (public.ran(VALUE));\nALTER TABLE public.books ADD COLUMN extra public.dom;\n"},
			{"a domain named public.text", "CREATE DOMAIN public.text AS pg_catalog.text;\nALTER TABLE public.books ADD COLUMN extra public.text;\n"},
			{"an enum", "CREATE TYPE public.mood AS ENUM ('a');\nALTER TABLE public.books ADD COLUMN extra public.mood;\n"},
			{"a composite type", "CREATE TYPE public.pair AS (a integer, b integer);\nALTER TABLE public.books ADD COLUMN extra public.pair;\n"},
			{"a built-in range", "ALTER TABLE public.books ADD COLUMN extra int4range;\n"},
			{"an array of a domain", "CREATE DOMAIN public.dom AS text;\nALTER TABLE public.books ADD COLUMN extra public.dom[];\n"},
		} {
			stepCase(t, c.name, c.setup, stepManifest(), true)
		}
		// The rule holds on each table of the manifest, and a listed column keeps the type of the manifest.
		plain := stepManifest()
		plain.Tables[1].Entity = false
		stepCase(t, "a table that is not the table of a declared entity", "CREATE DOMAIN public.dom AS text;\nALTER TABLE public.books ADD COLUMN extra public.dom;\n", plain, true)
		stepCase(t, "probe D1 against the manifest with no entity table", hostile+"CREATE DOMAIN public.dom AS text CHECK (public.ran(VALUE));\nALTER TABLE public.books ADD COLUMN extra public.dom;\n", want(), true)
		stepCase(t, "a text column on the manifest with no entity table", "ALTER TABLE public.books ADD COLUMN label text;\n", want(), false)
		listed := stepManifest()
		listed.Tables[1].Columns = append(listed.Tables[1].Columns, Column{Name: "extra", Type: "public.dom"})
		stepCase(t, "a listed column of the type of the manifest", "CREATE DOMAIN public.dom AS text;\nALTER TABLE public.books ADD COLUMN extra public.dom;\n", listed, false)
	})
}

func TestStepDefault(t *testing.T) {
	t.Run("ST-SUP-RP15-LC01-T09 Check accepts only a constant default of a built-in type on a column of an entity table that the manifest does not list", func(t *testing.T) {
		stepCase(t, "an integer constant", "ALTER TABLE public.books ADD COLUMN rank integer DEFAULT 0;\n", stepManifest(), false)
		stepCase(t, "a text constant", "ALTER TABLE public.books ADD COLUMN label text DEFAULT 'x';\n", stepManifest(), false)
		plain := stepManifest()
		plain.Tables[1].Entity = false
		stepCase(t, "a table that is not the table of a declared entity", "ALTER TABLE public.books ADD COLUMN rank integer DEFAULT 0;\n", plain, true)
		for _, c := range []struct{ name, setup string }{
			{"a function call", "ALTER TABLE public.books ADD COLUMN at timestamptz DEFAULT now();\n"},
			{"a sequence", "CREATE SEQUENCE public.s; ALTER TABLE public.books ADD COLUMN n bigint DEFAULT nextval('public.s');\n"},
			{"a cast through a user type", "CREATE DOMAIN public.d AS text; ALTER TABLE public.books ADD COLUMN v public.d DEFAULT 'x'::public.d;\n"},
			{"an expression", "ALTER TABLE public.books ADD COLUMN r integer DEFAULT 1 + 1;\n"},
			{"a constant on a column of a built-in type that is not on the list of builtinType", "ALTER TABLE public.books ADD COLUMN o oid DEFAULT 0;\n"},
			{"a listed column", "ALTER TABLE public.books ALTER COLUMN pages SET DEFAULT 1;\n"},
		} {
			stepCase(t, c.name, c.setup, stepManifest(), true)
		}
	})
}
