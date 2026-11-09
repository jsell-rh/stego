package declaration

import (
	"fmt"

	"github.com/jsell-rh/stego/internal/types"
)

func queryPath(i int) string { return fmt.Sprintf("queries[%d]", i) }

// validateQueryNames checks that each query's name is unique in the declaration and is not the name of a command
// of the declaration (ST-DECL-INV43).
func validateQueryNames(queries []types.Query, commands []types.Command) []ValidationError {
	commandNames := map[string]bool{}
	for _, command := range commands {
		commandNames[command.Name] = true
	}
	var faults []ValidationError
	first := map[string]int{}
	for i, query := range queries {
		switch j, used := first[query.Name]; {
		case query.Name == "":
			faults = append(faults, ValidationError{Category: "query", Rule: "query-name", Path: queryPath(i) + ".name", Message: "the query has no name; give it a name that is unique in the declaration"})
		case used:
			faults = append(faults, ValidationError{Category: "query", Rule: "query-name", Path: queryPath(i) + ".name", Found: query.Name, Message: fmt.Sprintf("the query name %q is used twice, by %s and %s; give each query its own name", query.Name, queryPath(j), queryPath(i))})
		case commandNames[query.Name]:
			faults = append(faults, ValidationError{Category: "query", Rule: "query-name", Path: queryPath(i) + ".name", Found: query.Name, Message: fmt.Sprintf("the query name %q is the name of a command; give the query a name that no command uses", query.Name)})
		default:
			first[query.Name] = i
		}
	}
	return faults
}

// validateQueryReads checks that each query names at least one entity it reads, and only entities that the
// declaration declares (ST-DECL-INV44).
func validateQueryReads(queries []types.Query, entities []types.Entity) []ValidationError {
	var faults []ValidationError
	for i, query := range queries {
		faults = append(faults, entityFaults("query", query.Name, queryPath(i), "reads", query.Reads, entities)...)
	}
	return faults
}

// validateQueryFill checks that each query names exactly one fill and that the fill implements the query slot
// (ST-DECL-INV45).
func validateQueryFill(queries []types.Query, facts Facts) []ValidationError {
	var faults []ValidationError
	for i, query := range queries {
		faults = append(faults, fillFault("query", query.Name, queryPath(i), query.Fill, QuerySlot, facts)...)
	}
	return faults
}
