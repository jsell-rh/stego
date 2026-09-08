package gen

import (
	_ "embed"
	"fmt"
)

// Contract identifies a compiler-owned interface version.
type Contract string

const StorageV1 Contract = "storage/v1"
const StorageContractNamespace = "contracts/storage"
const EventsV1 Contract = "events/v1"
const EventsContractNamespace = "contracts/events"

// ContractDefinition contains files and dependencies owned by the compiler.
type ContractDefinition struct {
	Files         []File
	GoModRequires map[string]string
}

//go:embed storage_contract.go.tmpl
var storageContractSource string

//go:embed events_contract.go.tmpl
var eventsContractSource string

func ResolveContract(id Contract) (ContractDefinition, error) {
	switch id {
	case EventsV1:
		return ContractDefinition{Files: []File{{Path: EventsContractNamespace + "/events.go", Content: []byte(eventsContractSource)}}, GoModRequires: map[string]string{"github.com/google/uuid": "v1.6.0"}}, nil
	case StorageV1:
		return ContractDefinition{
			Files:         []File{{Path: StorageContractNamespace + "/storage.go", Content: []byte(storageContractSource)}},
			GoModRequires: map[string]string{"github.com/google/uuid": "v1.6.0"},
		}, nil
	default:
		return ContractDefinition{}, fmt.Errorf("unsupported shared contract %q", id)
	}
}
