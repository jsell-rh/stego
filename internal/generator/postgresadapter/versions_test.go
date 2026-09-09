package postgresadapter

import (
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/types"
)

func TestVersionMetadataCannotBeDeclaredAsDomainFields(t *testing.T) {
	for _, name := range []string{"stego_revision", "StegoRevision", "resource_version", "ResourceVersion"} {
		ctx := gen.Context{OutputNamespace: "storage", Entities: []types.Entity{{Name: "Record", Versioned: true, Fields: []types.Field{{Name: name, Type: types.FieldTypeInt64}}}}}
		if _, _, err := new(Generator).Generate(ctx); err == nil {
			t.Fatal("accepted revision field collision", name)
		}
	}
}
