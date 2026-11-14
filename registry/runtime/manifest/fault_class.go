package manifest

import (
	"slices"
	"strings"
)

// FaultClasses are the classes of FaultClass, from the class that touches the tenant wall most to the least: the
// values of the field class of schema.manifest.mismatch.
var FaultClasses = []string{"rls", "policy", "grant", "owner", "trigger", "function", "rule", "column", "other"}

// FaultClass gives the class of a fault of Check, so that the alert of a start whose database does not satisfy the
// manifest names it (decision on finding F10 of the schema-change research, owner, 2026-11-13, and the orchestrator
// under the owner's delegation, 2026-11-14): rls for row-level security that is not enabled and forced; policy for a
// policy; grant for a privilege of a table, a column, a partition or a function; owner for the owner of an object, a
// confined role, a member of a confined role and the search path; trigger for a trigger and the
// session_replication_role that skips them; function for a function of the manifest; rule for a rule; column for a
// column, its default and its identity; and other for the rest, such as a constraint, an index or the kind of a
// table. It reads the fixed text of the fault after the name of the table or the schema of the manifest that it
// begins with, and after the name of a partition (review of PR #305, M3, orchestrator under the owner's delegation,
// 2026-11-14). The name of a partition is the catalog's and may hold ": ", so the text after each ": " of it is
// read, and the class that comes first in FaultClasses is given: a name can make the class of its fault only more
// severe.
func FaultClass(fault string) string {
	rest := fault
	if strings.HasPrefix(fault, "table ") || strings.HasPrefix(fault, "schema ") {
		if _, after, ok := strings.Cut(fault, ": "); ok {
			rest = after
		}
	}
	if !strings.HasPrefix(rest, "partition ") {
		return faultClass(rest)
	}
	class := "other"
	for at := rest; ; {
		_, after, ok := strings.Cut(at, ": ")
		if !ok {
			return class
		}
		if c := faultClass(after); slices.Index(FaultClasses, c) < slices.Index(FaultClasses, class) {
			class = c
		}
		at = after
	}
}

// faultClass gives the class of the fixed text of a fault.
func faultClass(rest string) string {
	switch {
	case strings.HasPrefix(rest, "row-level security "):
		return "rls"
	case strings.HasPrefix(rest, "policy "):
		return "policy"
	case strings.HasPrefix(rest, "the privilege ") || strings.Contains(rest, " may be run by roles ") ||
		strings.Contains(rest, " holds a privilege ") || strings.HasPrefix(rest, "the partition grants a privilege "):
		return "grant"
	case strings.Contains(rest, "belongs to the role ") || strings.HasPrefix(rest, "the confined role ") ||
		strings.Contains(rest, " is a member of the confined role ") || strings.Contains(rest, " has another owner") ||
		strings.Contains(rest, " on the search path of the session") || strings.Contains(rest, " has the name of the role of the session") ||
		strings.Contains(rest, " gets search_path from a setting "):
		return "owner"
	case strings.HasPrefix(rest, "trigger ") || strings.Contains(rest, "session_replication_role"):
		return "trigger"
	case strings.HasPrefix(rest, "function "):
		return "function"
	case strings.HasPrefix(rest, "rule ") || strings.HasPrefix(rest, "the partition has a rule"):
		return "rule"
	case strings.HasPrefix(rest, "column "):
		return "column"
	}
	return "other"
}
