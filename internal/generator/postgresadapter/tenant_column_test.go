package postgresadapter

import (
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/types"
)

func TestTenantColumn(t *testing.T) {
	t.Run("ST-SUP-RC01-OC40-T03 A field with the key's name and a reference into an owned entity from a shared one are refused", func(t *testing.T) {
		base := func(widget types.Entity, catalog types.Entity) gen.Context {
			return gen.Context{OutputNamespace: "storage", ModuleName: "example.com/tenancy-check", Tenancy: &gen.Tenancy{Key: "tenant_id"},
				PeerNamespaces: map[string]string{gen.TenancyComponent: "tenancy", "command": "commands", "query": "queries"}, ComponentConfig: map[string]any{"schema_generation": "tenancy-v1"},
				Entities: []types.Entity{widget, catalog}}
		}
		widget := types.Entity{Name: "Widget", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString, Unique: true}}}
		catalog := types.Entity{Name: "Catalog", Tenant: "shared", Fields: []types.Field{{Name: "code", Type: types.FieldTypeString}}}
		if _, _, err := new(Generator).Generate(base(widget, catalog)); err != nil {
			t.Fatalf("the declaration itself is refused: %v", err)
		}
		keyed := widget
		keyed.Fields = append([]types.Field{{Name: "tenant_id", Type: types.FieldTypeString}}, widget.Fields...)
		files, _, err := new(Generator).Generate(base(keyed, catalog))
		if err == nil || !strings.Contains(err.Error(), "Widget") || !strings.Contains(err.Error(), `"tenant_id"`) || files != nil {
			t.Errorf("a field with the key's name: files %d, error %v", len(files), err)
		}
		referring := catalog
		referring.Fields = append([]types.Field{{Name: "widget_id", Type: types.FieldTypeRef, To: "Widget"}}, catalog.Fields...)
		files, _, err = new(Generator).Generate(base(widget, referring))
		if err == nil || !strings.Contains(err.Error(), "Catalog") || !strings.Contains(err.Error(), `"widget_id"`) || files != nil {
			t.Errorf("a reference from a shared entity into an owned one: files %d, error %v", len(files), err)
		}
		plain := base(keyed, catalog)
		plain.Tenancy = nil
		delete(plain.PeerNamespaces, gen.TenancyComponent)
		if _, _, err := new(Generator).Generate(plain); err != nil {
			t.Errorf("a service without pooled tenancy may declare tenant_id: %v", err)
		}
	})
}
