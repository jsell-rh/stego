package postgresadapter_test

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/postgresadapter"
	"github.com/jsell-rh/stego/internal/generator/tenancy"
	"github.com/jsell-rh/stego/internal/types"
)

// boardEntities are the entities of the release before the add steps of ST-SUP-RC01-OC22-T02: the owned Board with a
// title, which a collection lists.
func boardEntities() []types.Entity {
	return []types.Entity{{Name: "Board", Fields: []types.Field{{Name: "title", Type: types.FieldTypeString}}}}
}

// boardCollections list the boards, and with withCard the cards, so that each listed table has search indexes.
func boardCollections(withCard bool) []types.Collection {
	collections := []types.Collection{{Name: "boards", Entity: "Board", Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpList}}}
	if withCard {
		collections = append(collections, types.Collection{Name: "cards", Entity: "Card", Operations: []types.Operation{types.OpCreate, types.OpList}})
	}
	return collections
}

// boardSteps is the release with three add steps on boardEntities: the owned entity Card, which refers to a Board
// and has an optional unique code; the optional field Board.note with a maximum length, which the list of boards
// searches; and the field Board.rank with a default.
func boardSteps(ns, tenancyNS string) gen.Context {
	n := func(v int) *int { return &v }
	entities := boardEntities()
	entities[0].Fields = append(entities[0].Fields,
		types.Field{Name: "note", Type: types.FieldTypeString, Optional: true, MinLength: n(1), MaxLength: n(40)},
		types.Field{Name: "rank", Type: types.FieldTypeInt32, Default: 0})
	entities = append(entities, types.Entity{Name: "Card", Fields: []types.Field{
		{Name: "board_id", Type: types.FieldTypeRef, To: "Board"},
		{Name: "code", Type: types.FieldTypeString, Optional: true, Unique: true},
	}})
	ctx := runtimeService(ns, tenancyNS, entities, boardCollections(true))
	ctx.SchemaChanges = []types.SchemaChange{
		{ID: "add-card", Kind: "add", Entity: "Card"},
		{ID: "add-board-note", Kind: "add", Entity: "Board", Field: "note"},
		{ID: "add-board-rank", Kind: "add", Entity: "Board", Field: "rank"},
	}
	ctx.SchemaHistory = []types.SchemaStep{{Change: "add-card", Phase: "expand"}, {Change: "add-board-note", Phase: "expand"}, {Change: "add-board-rank", Phase: "expand"}}
	return ctx
}

// boardBase is the release before boardSteps, as the base ref holds it.
func boardBase(ns, tenancyNS string) gen.Context {
	return runtimeService(ns, tenancyNS, boardEntities(), boardCollections(false))
}

// tenancyFiles generates the tenancy component of the service ctx into its tenancy namespace.
func tenancyFiles(t *testing.T, ctx gen.Context) []gen.File {
	t.Helper()
	own := ctx
	own.OutputNamespace = own.PeerNamespaces[gen.TenancyComponent]
	own.ComponentConfig = map[string]any{}
	files, _, err := new(tenancy.Generator).Generate(own)
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestSchemaSteps(t *testing.T) {
	t.Run("ST-SUP-RC01-OC22-T02 Each add step gives one migration after the baseline, in order, from the declaration alone", func(t *testing.T) {
		files, _, err := new(postgresadapter.Generator).Generate(boardSteps("boards", "boardtenancy"))
		if err != nil {
			t.Fatal(err)
		}
		names := []string{"step_0001_add-card_expand", "step_0002_add-board-note_expand", "step_0003_add-board-rank_expand"}
		migrate := generated(t, files, "boards/migrate.go")
		registered := regexp.MustCompile(`Register(?:SQL|Step)\("([^"]+)"`).FindAllStringSubmatch(migrate, -1)
		var order []string
		for _, match := range registered {
			order = append(order, match[1])
		}
		if want := append([]string{"001_initial", "100_tenancy_row_security"}, names...); !slices.Equal(order, want) {
			t.Errorf("migrate.go registers %v, want %v", order, want)
		}
		if !slices.IsSorted(order) {
			t.Errorf("the byte order of the names %v is not the order of the steps", order)
		}

		// 001_initial is the baseline: the body of the release before, so its ledger digest does not change.
		before, _, err := new(postgresadapter.Generator).Generate(boardBase("boards", "boardtenancy"))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := generated(t, files, "boards/migrations/001_initial.sql"), generated(t, before, "boards/migrations/001_initial.sql"); got != want {
			t.Errorf("001_initial of the release with the add steps is not the baseline:\n got %s\nwant %s", got, want)
		}
		if got, want := generated(t, tenancyFiles(t, boardSteps("boards", "boardtenancy")), "boardtenancy/migrations/row_security.sql"),
			generated(t, tenancyFiles(t, boardBase("boards", "boardtenancy")), "boardtenancy/migrations/row_security.sql"); got != want {
			t.Errorf("the row-level security of the tenancy component is not that of the baseline:\n got %s\nwant %s", got, want)
		}

		card := generated(t, files, "boards/migrations/"+names[0]+".sql")
		// The step that adds Card gives its table the text that 001_initial and the tenancy component give a Card of
		// the baseline, byte for byte.
		baseline := runtimeService("boards", "boardtenancy", boardSteps("boards", "boardtenancy").Entities[1:], nil)
		baseline.Entities = append(boardEntities(), baseline.Entities...)
		baseline.Collections = boardCollections(true)
		together, _, err := new(postgresadapter.Generator).Generate(baseline)
		if err != nil {
			t.Fatal(err)
		}
		initial := generated(t, together, "boards/migrations/001_initial.sql")
		cardTable := initial[strings.Index(initial, `CREATE TABLE public."cards"`):]
		security := generated(t, tenancyFiles(t, baseline), "boardtenancy/migrations/row_security.sql")
		cardSecurity := security[strings.Index(security, `ALTER TABLE public."cards"`):]
		if card != cardTable+cardSecurity {
			t.Errorf("the step of Card is not the table and the row-level security of a Card of the baseline:\n got %s\nwant %s%s", card, cardTable, cardSecurity)
		}
		if !strings.Contains(migrate, `RegisterStep("step_0001_add-card_expand", `) {
			t.Errorf("the step of Card is not a step migration with its compatible_with set:\n%s", migrate)
		}

		for name, want := range map[string]string{
			names[1]: "ALTER TABLE public.\"boards\" ADD COLUMN \"note\" character varying(40);\n" +
				"ALTER TABLE public.\"boards\" ADD CONSTRAINT \"boards_note_check\" CHECK (char_length(\"note\") >= 1);\n" +
				"CREATE INDEX \"boards_note_search\" ON public.\"boards\" (\"tenant_id\", \"note\");\n",
			names[2]: "ALTER TABLE public.\"boards\" ADD COLUMN \"rank\" integer NOT NULL DEFAULT 0;\n" +
				"CREATE INDEX \"boards_rank_search\" ON public.\"boards\" (\"tenant_id\", \"rank\");\n",
		} {
			if got := generated(t, files, "boards/migrations/"+name+".sql"); got != want {
				t.Errorf("%s is\n%s\nwant\n%s", name, got, want)
			}
			if !strings.Contains(migrate, `RegisterStep("`+name+`", `) {
				t.Errorf("migrate.go does not register %s", name)
			}
		}

		again, _, err := new(postgresadapter.Generator).Generate(boardSteps("boards", "boardtenancy"))
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			if got := generated(t, again, file.Path); got != string(file.Content) {
				t.Errorf("the same declaration gives another %s", file.Path)
			}
		}
	})

	t.Run("ST-SUP-RC01-OC22-T02 Generation refuses a step that is not the expand step of an add, or an add that the rows cannot take", func(t *testing.T) {
		refused := map[string]func(*gen.Context){
			"rename": func(ctx *gen.Context) {
				ctx.Entities[0].Fields[0].Name = "heading"
				ctx.SchemaChanges = append(ctx.SchemaChanges, types.SchemaChange{ID: "board-title-rename", Kind: "rename", Entity: "Board", Field: "heading", From: "title"})
				ctx.SchemaHistory = append(ctx.SchemaHistory, types.SchemaStep{Change: "board-title-rename", Phase: "expand"})
			},
			"is not built": func(ctx *gen.Context) {
				ctx.SchemaHistory = append(ctx.SchemaHistory, types.SchemaStep{Change: "add-card", Phase: "contract"})
			},
			"NOT NULL with no default": func(ctx *gen.Context) { ctx.Entities[0].Fields[2].Default = nil },
			"which a later step adds": func(ctx *gen.Context) {
				ctx.Entities[1].Fields = ctx.Entities[1].Fields[1:]
				ctx.Entities[0].Fields = append(ctx.Entities[0].Fields, types.Field{Name: "card_id", Type: types.FieldTypeRef, To: "Card", Optional: true})
				ctx.SchemaChanges = append(ctx.SchemaChanges, types.SchemaChange{ID: "add-board-card-id", Kind: "add", Entity: "Board", Field: "card_id"})
				ctx.SchemaHistory = append([]types.SchemaStep{{Change: "add-board-card-id", Phase: "expand"}}, ctx.SchemaHistory...)
			},
			"has no step": func(ctx *gen.Context) { ctx.SchemaHistory = ctx.SchemaHistory[1:] },
		}
		for want, change := range refused {
			ctx := boardSteps("boards", "boardtenancy")
			change(&ctx)
			files, _, err := new(postgresadapter.Generator).Generate(ctx)
			if err == nil || !strings.Contains(err.Error(), want) || files != nil {
				t.Errorf("%s: files %d, error %v; want a refusal that says %q", want, len(files), err, want)
			}
		}
	})
}

// markerOf gives the two marker digests that the schema_generation.go of the namespace ns holds.
func markerOf(t *testing.T, files []gen.File, ns string) (string, string) {
	t.Helper()
	source := generated(t, files, ns+"/schema_generation.go")
	definition := regexp.MustCompile(`const SchemaDefinition = "([0-9a-f]{64})"`).FindStringSubmatch(source)
	before := regexp.MustCompile(`const schemaDefinitionBefore = "([0-9a-f]{64})"`).FindStringSubmatch(source)
	if definition == nil || before == nil {
		t.Fatalf("schema_generation.go holds no two marker digests:\n%s", source)
	}
	return definition[1], before[1]
}

func TestSchemaMarker(t *testing.T) {
	t.Run("ST-SUP-RC01-OC42-T01 The marker digest changes with the stored shape and not with a collection or a permission", func(t *testing.T) {
		generate := func(ctx gen.Context) (string, string) {
			t.Helper()
			files, _, err := new(postgresadapter.Generator).Generate(ctx)
			if err != nil {
				t.Fatal(err)
			}
			return markerOf(t, files, "boards")
		}
		definition, before := generate(boardBase("boards", "boardtenancy"))
		if definition == before {
			t.Error("the digest of the stored shape is the digest that a release before wrote")
		}
		if steps, _ := generate(boardSteps("boards", "boardtenancy")); steps != definition {
			t.Error("the add steps change the marker digest")
		}
		listed := boardBase("boards", "boardtenancy")
		listed.Collections = append(listed.Collections, types.Collection{Name: "boards-by-title", Entity: "Board", Operations: []types.Operation{types.OpList, types.OpUpsert}, UpsertKey: []string{"title"}, Implicit: map[string]string{"title": "x"}})
		listed.Entities[0].ResourceType, listed.Entities[0].Permissions = "", map[types.Operation]string{types.OpRead: "public"}
		if got, gotBefore := generate(listed); got != definition || gotBefore == before {
			t.Errorf("a collection and a permission change the digest of the stored shape (%v) or do not change the digest of a release before (%v)", got != definition, gotBefore == before)
		}
		for name, change := range map[string]func(*gen.Context){
			"a new column": func(ctx *gen.Context) {
				ctx.Entities[0].Fields = append(ctx.Entities[0].Fields, types.Field{Name: "note", Type: types.FieldTypeString, Optional: true})
			},
			"a NOT NULL":     func(ctx *gen.Context) { ctx.Entities[0].Fields[0].Optional = true },
			"a unique field": func(ctx *gen.Context) { ctx.Entities[0].Fields[0].Unique = true },
			"a protected entity": func(ctx *gen.Context) {
				ctx.Entities[0].ResourceType = "board"
			},
		} {
			ctx := boardBase("boards", "boardtenancy")
			change(&ctx)
			if got, _ := generate(ctx); got == definition {
				t.Errorf("%s does not change the digest of the stored shape", name)
			}
		}
	})

	t.Run("ST-SUP-RC01-OC43-T01 Generation refuses a ledgered migration that is not the baseline, the tenancy row-level security or a step", func(t *testing.T) {
		for _, name := range []string{"001_initial", "100_tenancy_row_security", "step_0001_add-card_expand", "step_0012_note-title-rename_contract"} {
			if err := postgresadapter.CheckLedgerName(name); err != nil {
				t.Errorf("%s is refused: %v", name, err)
			}
		}
		for _, name := range []string{"101_x", "002_audit", "200_outbox", "step_1_add-card_expand", "step_0001_add-card_backfill", "step_0001_Add_expand", "zz_late", ""} {
			if err := postgresadapter.CheckLedgerName(name); err == nil || !strings.Contains(err.Error(), "ST-SUP-RC01-OC43") {
				t.Errorf("%s is accepted, or its refusal does not name ST-SUP-RC01-OC43: %v", name, err)
			}
		}
		// A step name that the grammar refuses stops generation: the check reads every registration of migrate.go.
		ctx := boardSteps("boards", "boardtenancy")
		ctx.SchemaChanges[0].ID = "Add_Card"
		ctx.SchemaHistory[0].Change = "Add_Card"
		if files, _, err := new(postgresadapter.Generator).Generate(ctx); err == nil || !strings.Contains(err.Error(), "ST-SUP-RC01-OC43") || files != nil {
			t.Errorf("a step named step_0001_Add_Card_expand gives files %d and error %v, want the refusal", len(files), err)
		}
	})
}
