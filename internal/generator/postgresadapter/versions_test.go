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

func TestGenerateMultipleCleanupTargetOwners(t *testing.T) {
	for _, field := range []string{"region", "archive_region"} {
		ctx := gen.Context{ModuleName: "example.com/multi", OutputNamespace: "storage", StorageContract: "example.com/multi/contracts/storage", Entities: []types.Entity{{Name: "Asset", Versioned: true, CleanupOwners: []string{"compute", "archive"}, CleanupTargets: map[string]string{"compute": "region", "archive": field}, Fields: []types.Field{{Name: "region", Type: types.FieldTypeString}, {Name: "archive_region", Type: types.FieldTypeString}}}}}
		if _, _, err := new(Generator).Generate(ctx); err != nil {
			t.Fatal("multiple target owners did not generate valid source", field, err)
		}
	}
}
