package declaration

import (
	"gopkg.in/yaml.v3"

	"github.com/jsell-rh/stego/internal/types"
)

// Scaffold makes the declaration of a new service of the named archetype (ST-DECL-CMD01). The caller gives the
// facts of the archetype that it resolved from the registry: here, the archetype's language. The scaffold holds
// no example content: no entity and no collection.
func Scaffold(name, archetype, language string) ([]byte, error) {
	return yaml.Marshal(types.ServiceDeclaration{Kind: "service", Name: name, Archetype: archetype, Language: language, Entities: []types.Entity{}, Collections: []types.Collection{}})
}
