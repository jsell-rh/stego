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

// boardNoDefault is boardSteps with only its adds that give no default: the table Card and the optional field
// Board.note, and not Board.rank, whose default the release before refuses until ST-GEN-Q15 (a) ships.
func boardNoDefault(ns, tenancyNS string) gen.Context {
	ctx := boardSteps(ns, tenancyNS)
	fields := ctx.Entities[0].Fields
	ctx.Entities[0].Fields = fields[:len(fields)-1]
	ctx.SchemaChanges = ctx.SchemaChanges[:2]
	ctx.SchemaHistory = ctx.SchemaHistory[:2]
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
