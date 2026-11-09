package declaration

import (
	"strings"
	"testing"
)

// queryFacts are the facts of ST-DECL-TD02 with the fill open-reservations of the query slot, the fill
// reserve-and-charge of the command slot and the fill report of the query slot.
func queryFacts() Facts {
	facts := commandFacts("reserve-and-charge")
	facts.FillSlots["open-reservations"] = QuerySlot
	facts.FillSlots["report"] = QuerySlot
	return facts
}

// query writes one query of ST-DECL-TD02 with the lines given after its name.
func query(name string, lines ...string) string {
	return "  - name: " + name + "\n    " + strings.Join(lines, "\n    ") + "\n"
}

// standardQuery is a query that reads Todo, bound to the fill open-reservations.
func standardQuery(name string, lines ...string) string {
	return query(name, append([]string{"reads: [Todo]", "fill: open-reservations", "route: GET /reports/" + name}, lines...)...)
}

func TestQueryNames(t *testing.T) {
	facts := queryFacts()
	for _, c := range []struct{ name, service string }{
		{"two queries named open-reservations", td02 + "queries:\n" + standardQuery("open-reservations") + query("open-reservations", "reads: [Todo]", "fill: open-reservations", "route: GET /reports/other")},
		{"a query and a command named open-reservations", td02 + "queries:\n" + standardQuery("open-reservations") + "commands:\n" + standardCommand("open-reservations")},
	} {
		t.Run("ST-DECL-INV43-T A query name used twice, or used by a command, is refused: "+c.name, func(t *testing.T) {
			wantFault(t, compositionFaults(t, c.service, facts, "query-name"), `"open-reservations"`)
		})
	}
	t.Run("ST-DECL-INV43-T A query named reservations-report beside the command is accepted", func(t *testing.T) {
		service := td02 + "queries:\n" + standardQuery("reservations-report") + "commands:\n" + standardCommand("open-reservations")
		wantNoFault(t, compositionFaults(t, service, facts))
	})
}

func TestQueryReads(t *testing.T) {
	facts := queryFacts()
	with := func(reads string) string {
		return td02 + "queries:\n" + query("open-reservations", "reads: "+reads, "fill: open-reservations", "route: GET /reservations/open")
	}
	t.Run("ST-DECL-INV44-T A query that reads Todo is accepted", func(t *testing.T) {
		wantNoFault(t, compositionFaults(t, with("[Todo]"), facts))
	})
	t.Run("ST-DECL-INV44-T A query that reads no entity is refused", func(t *testing.T) {
		wantFault(t, compositionFaults(t, with("[]"), facts, "query-reads"), `"open-reservations"`)
	})
	t.Run("ST-DECL-INV44-T A query that reads an undeclared entity is refused", func(t *testing.T) {
		wantFault(t, compositionFaults(t, with("[Gadget]"), facts, "query-reads"), `"open-reservations"`, `"Gadget"`)
	})
}

func TestQueryFill(t *testing.T) {
	facts := queryFacts()
	with := func(fill string) string {
		return td02 + "queries:\n" + query("open-reservations", "reads: [Todo]", fill, "route: GET /reservations/open")
	}
	t.Run("ST-DECL-INV45-T A query with a fill of the query slot is accepted", func(t *testing.T) {
		wantNoFault(t, compositionFaults(t, with("fill: open-reservations"), facts))
	})
	for _, c := range []struct{ name, fill, names string }{
		{"no fill", "fill: []", "no fill"},
		{"two fills", "fill: [open-reservations, report]", `"report"`},
		{"a fill of the command slot", "fill: reserve-and-charge", `"reserve-and-charge"`},
	} {
		t.Run("ST-DECL-INV45-T A query with "+c.name+" is refused", func(t *testing.T) {
			wantFault(t, compositionFaults(t, with(c.fill), facts, "query-fill"), `"open-reservations"`, c.names)
		})
	}
}
