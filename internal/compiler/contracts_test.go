package compiler

import (
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/types"
)

func TestSharedContractAssembly(t *testing.T) {
	input := AssemblerInput{ModuleName: "example.com/shared", GoVersion: "1.26.8", Wirings: []ComponentWiring{
		{Name: "first", Wiring: &gen.Wiring{Contracts: []gen.Contract{gen.StorageV1, gen.StorageV1}}},
		{Name: "second", Wiring: &gen.Wiring{Contracts: []gen.Contract{gen.StorageV1}}},
	}}
	files, err := Assemble(input)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, file := range files {
		if file.Path == gen.StorageContractNamespace+"/storage.go" {
			count++
		}
		if file.Path == "go.mod" && !strings.Contains(string(file.Content), "github.com/google/uuid v1.6.0") {
			t.Fatal("shared contract dependency is missing")
		}
	}
	if count != 1 {
		t.Fatalf("shared contract emitted %d times", count)
	}
	input.Wirings[0].Wiring.Contracts = []gen.Contract{"storage/unknown"}
	if _, err := Assemble(input); err == nil {
		t.Fatal("unknown contract version accepted")
	}
}

func TestSharedContractNamespaceIsReserved(t *testing.T) {
	for _, ns := range []string{"contracts", "contracts/storage", "Contracts/Custom"} {
		errors := validateComponentNamespaces(map[string]*types.Component{"custom": {OutputNamespace: ns}})
		if len(errors) == 0 {
			t.Fatalf("component accepted compiler namespace %q", ns)
		}
	}
}
