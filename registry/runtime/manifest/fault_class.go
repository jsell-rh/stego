package manifest

import "strings"

// FaultClass gives the class of a fault of Check, so that the alert of a start whose database does not satisfy the
// manifest names it (decision on finding F10 of the schema-change research, owner, 2026-11-13, and the orchestrator
// under the owner's delegation, 2026-11-14): rls for row-level security that is not enabled and forced; policy for a
// policy; grant for a privilege of a table, a column or a function; owner for the owner of an object, a confined
// role and the search path; trigger for a trigger and the session_replication_role that skips them; function for a
// function of the manifest; rule for a rule; column for a column, its default and its identity; and other for the
// rest, such as a constraint, an index or the kind of a table. It reads the fixed text of the fault after the name
// of the table or the schema of the manifest that it begins with.
func FaultClass(fault string) string {
	rest := fault
	if strings.HasPrefix(fault, "table ") || strings.HasPrefix(fault, "schema ") {
		if _, after, ok := strings.Cut(fault, ": "); ok {
			rest = after
		}
	}
	switch {
	case strings.HasPrefix(rest, "row-level security "):
		return "rls"
	case strings.HasPrefix(rest, "policy "):
		return "policy"
	case strings.HasPrefix(rest, "the privilege ") || strings.Contains(rest, " may be run by roles ") ||
		strings.Contains(rest, " holds a privilege "):
		return "grant"
	case strings.Contains(rest, "belongs to the role ") || strings.HasPrefix(rest, "the confined role ") ||
		strings.Contains(rest, " on the search path of the session") || strings.Contains(rest, " has the name of the role of the session"):
		return "owner"
	case strings.HasPrefix(rest, "trigger ") || strings.Contains(rest, "session_replication_role"):
		return "trigger"
	case strings.HasPrefix(rest, "function "):
		return "function"
	case strings.HasPrefix(rest, "rule "):
		return "rule"
	case strings.HasPrefix(rest, "column "):
		return "column"
	}
	return "other"
}
