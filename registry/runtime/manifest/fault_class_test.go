package manifest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/registry/runtime/probe"
)

func TestFaultClass(t *testing.T) {
	t.Run("ST-SUP-RP15-LC01-T17 Each fault of Check has its class, and each class is a value of the field class of schema.manifest.mismatch", func(t *testing.T) {
		for fault, class := range map[string]string{
			"table public.orders: row-level security is not enabled and forced":                                                    "rls",
			"table public.orders: policy orders_tenant is missing":                                                                 "policy",
			"table public.orders: policy evil is a permissive policy that the manifest does not name":                              "policy",
			"table stego_outbox.messages: the privilege SELECT to PUBLIC is granted, and the manifest does not grant it":           "grant",
			"table public.orders: the privilege SELECT to svc_rt of the manifest is not granted":                                   "grant",
			"function public.audit_maintain may be run by roles that the manifest does not name":                                   "grant",
			"the confined role svc_relay holds a privilege on the relation public.orders, which the manifest does not name for it": "grant",
			"table public.orders: the table belongs to the role svc_relay, and the manifest needs svc_owner":                       "owner",
			"the confined role svc_relay owns the table public.x, which the manifest does not name for it":                         "owner",
			"the relation svc_rt.orders comes before public.orders on the search path of the session":                              "owner",
			"the schema svc_rt has the name of the role of the session, which a search path of \"$user\" puts first":               "owner",
			"table public.audit_event: trigger audit_event_reject_row is missing":                                                  "trigger",
			"the session runs with session_replication_role replica, which skips the triggers":                                     "trigger",
			"function public.audit_maintain is not the function of the manifest":                                                   "function",
			"table public.orders: rule r is a rule that the manifest does not allow":                                               "rule",
			"table public.orders: column status has the type character varying(20), and the manifest needs text":                   "column",
			"table public.orders: column status has a default that the manifest does not state":                                    "column",
			"table public.orders: index orders_pkey is missing":                                                                    "other",
			"table public.orders: constraint c is not validated":                                                                   "other",
			// A table name with the text of another class does not change the class: the class is read after it.
			"table public.\"policy x\": column a is missing": "column",
		} {
			if got := FaultClass(fault); got != class {
				t.Errorf("FaultClass(%q) = %q, want %q", fault, got, class)
			}
			if !slices.Contains(probe.SchemaFaultClasses, class) {
				t.Errorf("the class %q is not a value of the field class of schema.manifest.mismatch", class)
			}
		}
	})
}

// faultTemplates gives the template of each fault text that manifest.go makes: the text of the call of fault, of
// each fault appended to faults, of a fault in a slice literal or in the variable fault, and of each text of
// triggerFault after "trigger %s ". A part of the text that is not a constant is %v.
func faultTemplates(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "manifest.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var render func(e ast.Expr) (string, bool)
	render = func(e ast.Expr) (string, bool) {
		switch x := e.(type) {
		case *ast.BasicLit:
			if x.Kind != token.STRING {
				return "%v", true
			}
			s, err := strconv.Unquote(x.Value)
			return s, err == nil
		case *ast.BinaryExpr:
			if x.Op != token.ADD {
				return "", false
			}
			l, ok := render(x.X)
			if !ok {
				return "", false
			}
			r, ok := render(x.Y)
			return l + r, ok
		case *ast.CallExpr:
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Sprintf" && len(x.Args) > 0 {
				if lit, ok := x.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					return render(lit)
				}
			}
			return "%v", true
		}
		return "%v", true
	}
	var templates []string
	add := func(e ast.Expr, prefix string) {
		// The prefixes of a table, a schema and a partition, and the trigger with the text of triggerFault, are not
		// formats of their own.
		if s, ok := render(e); ok && s != "%s" && s != "%v" && s != "trigger %s %s" && !strings.HasSuffix(s, ": %v") && strings.Contains(s, " ") {
			templates = append(templates, prefix+s)
		}
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncDecl:
			if x.Name.Name == "triggerFault" {
				ast.Inspect(x.Body, func(n ast.Node) bool {
					if r, ok := n.(*ast.ReturnStmt); ok && len(r.Results) == 1 {
						if s, ok := render(r.Results[0]); ok && s != "" {
							templates = append(templates, "trigger %s "+s)
						}
					}
					return true
				})
				return false
			}
		case *ast.CallExpr:
			if id, ok := x.Fun.(*ast.Ident); ok {
				switch {
				case id.Name == "fault" && len(x.Args) > 0:
					if lit, ok := x.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						add(lit, "")
					}
				case id.Name == "append" && len(x.Args) == 2 && !x.Ellipsis.IsValid():
					if a, ok := x.Args[0].(*ast.Ident); ok && a.Name == "faults" {
						if _, sub := x.Args[1].(*ast.Ident); !sub {
							add(x.Args[1], "")
						}
					}
				}
			}
		case *ast.CompositeLit:
			if a, ok := x.Type.(*ast.ArrayType); ok {
				if id, ok := a.Elt.(*ast.Ident); ok && id.Name == "string" && len(x.Elts) == 1 {
					add(x.Elts[0], "")
				}
			}
		case *ast.AssignStmt:
			if len(x.Lhs) == 1 && len(x.Rhs) == 1 && x.Tok != token.ADD_ASSIGN {
				if id, ok := x.Lhs[0].(*ast.Ident); ok && id.Name == "fault" {
					add(x.Rhs[0], "")
				}
			}
		}
		return true
	})
	slices.Sort(templates)
	return slices.Compact(templates)
}

// templateText gives the regular expression of a template, which a whole fault matches after the prefixes of its
// table, its schema and its partition.
func templateText(template string) *regexp.Regexp {
	var b strings.Builder
	for i, part := range regexp.MustCompile(`%[sdvq]`).Split(template, -1) {
		if i > 0 {
			b.WriteString(".+")
		}
		b.WriteString(regexp.QuoteMeta(part))
	}
	return regexp.MustCompile(`^(?:(?:table|schema) [^:]+: )?(?:partition .+: )?` + b.String() + `$`)
}

func TestFaultClassFormats(t *testing.T) {
	t.Run("ST-SUP-RP15-LC01-T18 Each fault format of manifest.go has its class, after the prefix of a table, a schema and a partition", func(t *testing.T) {
		templates := faultTemplates(t)
		var found []string
		for _, sample := range slices.Sorted(maps.Keys(faultSamples)) {
			found = append(found, sample)
		}
		for _, template := range templates {
			re := templateText(template)
			matched := false
			for sample := range faultSamples {
				if re.MatchString(sample) {
					matched = true
					break
				}
			}
			if !matched {
				t.Errorf("no sample of FaultClass has the format %q", template)
			}
		}
		for sample, class := range faultSamples {
			if got := FaultClass(sample); got != class {
				t.Errorf("FaultClass(%q) = %q, want %q", sample, got, class)
			}
		}
		if !slices.Equal(FaultClasses, probe.SchemaFaultClasses) {
			t.Errorf("FaultClasses %v are not the values %v of the field class of schema.manifest.mismatch", FaultClasses, probe.SchemaFaultClasses)
		}
		t.Logf("%d formats, %d samples", len(templates), len(found))
	})
}

// faultSamples is a fault of each format of manifest.go, as Check gives it, with its class.
var faultSamples = map[string]string{
	"table public.orders: row-level security is not enabled and forced": "rls",

	"table public.orders: policy orders_tenant is missing":                                                              "policy",
	"table public.orders: policy orders_tenant is not the policy of the manifest":                                       "policy",
	"table public.orders: policy orders_tenant applies to other roles than the manifest names":                          "policy",
	"table public.orders: policy evil is a permissive policy that the manifest does not name":                           "policy",
	"table public.orders: policy evil is a restrictive policy that the manifest does not name":                          "policy",
	"table public.audit_event: partition public.audit_event_p202611: the partition has a rule":                          "rule",
	"table public.audit_event: partition public.audit_event_p202611: rule r is a rule that the manifest does not allow": "rule",
	"table public.orders: rule r is a rule that the manifest does not allow":                                            "rule",

	"table stego_outbox.messages: the privilege SELECT to PUBLIC is granted, and the manifest does not grant it":                                           "grant",
	"table public.orders: the privilege SELECT to svc_rt of the manifest is not granted":                                                                   "grant",
	"table public.orders: the privilege SELECT (status) to svc_rt is granted with the grant option":                                                        "grant",
	"schema stego_outbox: the privilege USAGE to svc_rt is granted with the grant option":                                                                  "grant",
	"schema stego_outbox: the privilege USAGE to svc_rt is granted, and the manifest does not grant it":                                                    "grant",
	"schema stego_outbox: the privilege USAGE to svc_rt of the manifest is not granted":                                                                    "grant",
	"schema public: the privilege CREATE to svc_rt is granted, and the manifest gives it to no role but the owner":                                         "grant",
	"function public.audit_maintain() may be run by roles that the manifest does not name":                                                                 "grant",
	"the confined role svc_relay holds a privilege on the relation public.orders, which the manifest does not name for it":                                 "grant",
	"table public.audit_event: partition public.audit_event_p202611: the partition grants a privilege on it or on a column to another role than its owner": "grant",

	"table public.orders: the table belongs to the role svc_relay, and the manifest needs svc_owner":                                          "owner",
	"schema stego_outbox: the schema belongs to the role svc_relay, and the manifest needs svc_owner":                                         "owner",
	"table public.audit_event: partition public.audit_event_p202611: the partition belongs to the role x, and the table belongs to svc_owner": "owner",
	"function public.audit_maintain() has another owner":                                                                                      "owner",
	"the confined role svc_relay is missing":                                                                                                  "owner",
	"the confined role svc_relay owns the table public.x, which the manifest does not name for it":                                            "owner",
	"the confined role svc_relay is a member of the role svc_owner, which the manifest does not name for it":                                  "owner",
	// Review of PR #305, M3: a member of a confined role gets the rows of every tenant through its functions.
	"the role svc_rt is a member of the confined role svc_relay, which the manifest grants only to its owner WITH SET TRUE, INHERIT FALSE and no ADMIN":                                                                                                                 "owner",
	"the role x is a member of the confined role svc_relay, which the manifest grants only to its owner WITH SET TRUE, INHERIT FALSE and no ADMIN; a superuser runs REVOKE \"svc_relay\" FROM \"x\" GRANTED BY \"postgres\", or the role migration runs as a superuser": "owner",
	"the relation svc_rt.orders comes before public.orders on the search path of the session":                                                                                                                                                                           "owner",
	// Review of PR #305, M2: the migration job reads the settings that a new session of the runtime role gets.
	"the role svc_rt gets search_path from a setting of the role, which the manifest does not allow":         "owner",
	"the role svc_rt gets session_replication_role from a setting of the database, which skips the triggers": "trigger",
	"the schema svc_rt has the name of the role of the session, which a search path of \"$user\" puts first": "owner",

	"table public.audit_event: trigger audit_event_reject_row is missing":                                                       "trigger",
	"table public.audit_event: trigger audit_event_reject_row is not the trigger of the manifest":                               "trigger",
	"table public.audit_event: trigger audit_event_reject_row is disabled":                                                      "trigger",
	"table public.audit_event: trigger t runs the function public.f, which is not the function of the manifest":                 "trigger",
	"table public.audit_event: trigger t runs the function public.f, which the role svc_owner does not own":                     "trigger",
	"table public.audit_event: trigger t runs the function public.f, which the owner of the table does not own":                 "trigger",
	"table public.orders: trigger evil is a trigger that the manifest does not name":                                            "trigger",
	"table public.orders: trigger s2 is one of more than one sync trigger that the manifest does not name":                      "trigger",
	"table public.audit_event: partition public.audit_event_p202611: trigger evil is a trigger that the manifest does not name": "trigger",
	"table public.audit_event: partition public.audit_event_p202611: trigger t is disabled":                                     "trigger",
	"table public.audit_event: partition public.audit_event_p202611: trigger t is missing":                                      "trigger",
	"table public.audit_event: partition public.audit_event_p202611: trigger t is not the trigger of the manifest":              "trigger",
	"the session runs with session_replication_role replica, which skips the triggers":                                          "trigger",

	"function public.audit_maintain() is missing":                                                           "function",
	"function public.audit_maintain() is not the function of the manifest":                                  "function",
	"function public.audit_maintain() has another strictness, volatility or result than the manifest":       "function",
	"function public.audit_maintain(text) has the name of a function of the manifest, and is not one of it": "function",
	"schema stego_outbox: function stego_outbox.f() is a function that the manifest does not name":          "function",

	"table public.orders: column status is missing":                                                                                                       "column",
	"table public.orders: column status has the type character varying(20), and the manifest needs text":                                                  "column",
	"table public.orders: column status is nullable, and the manifest needs NOT NULL":                                                                     "column",
	"table public.orders: column status has the collation x, and the manifest needs the default or the C collation":                                       "column",
	"table public.orders: column extra is not in the manifest and has the type x, which is no built-in base type of pg_catalog":                           "column",
	"table public.orders: column status has a default that the manifest does not state":                                                                   "column",
	"table public.orders: column status has a default that the manifest does not state, and that is not a constant of a built-in type":                    "column",
	"table public.orders: column status has another default than the manifest states":                                                                     "column",
	"table public.orders: column status has no default, and the manifest states one":                                                                      "column",
	"table public.orders: column g is a generated column, which the manifest does not state":                                                              "column",
	"table public.orders: column n is an identity that the manifest does not state":                                                                       "column",
	"table public.orders: column n is another identity than the manifest states":                                                                          "column",
	"table public.orders: column n is an identity with no sequence":                                                                                       "column",
	"table public.orders: column n is an identity whose sequence does not count up by 1 from 1 to the maximum of its type with a cache of 1 and no cycle": "column",
	"table public.orders: column n is no identity, and the manifest states one":                                                                           "column",

	"table public.orders is missing": "other",
	"schema stego_outbox is missing": "other",
	"table public.orders: the table is partitioned, and the manifest needs a plain table":                          "other",
	"table public.audit_event: the table is not partitioned, and the manifest needs a partitioned table":           "other",
	"table public.orders: the table is a partition or a child of another table":                                    "other",
	"table public.orders: another table inherits from the table":                                                   "other",
	"table public.orders: constraint c is a foreign key that the manifest does not name":                           "other",
	"table public.orders: constraint c is a check constraint that the manifest does not name":                      "other",
	"table public.orders: constraint c is an exclusion constraint that the manifest does not name":                 "other",
	"table public.orders: the foreign key public.x.c refers to the table with an action that writes":               "other",
	"table public.orders: constraint c is missing":                                                                 "other",
	"table public.orders: constraint c is not the constraint of the manifest":                                      "other",
	"table public.orders: constraint c is not validated":                                                           "other",
	"table public.orders: index orders_pkey is missing":                                                            "other",
	"table public.orders: index i is not unique":                                                                   "other",
	"table public.orders: index i is unique, and the manifest needs a lookup index":                                "other",
	"table public.orders: index i does not have the columns (tenant_id, id) of the manifest":                       "other",
	"table public.orders: index i does not have the predicate of the manifest":                                     "other",
	"table public.orders: index i is not valid":                                                                    "other",
	"table public.orders: index i is deferrable":                                                                   "other",
	"table public.orders: index i treats nulls as not distinct":                                                    "other",
	"table public.orders: index i has an operator class or a collation that is not the default":                    "other",
	"table public.orders: index i has an expression or a predicate, and the manifest does not name it":             "other",
	"table public.orders: index i is a unique index that the manifest does not name":                               "other",
	"table public.audit_event: partition public.audit_event_p202611: the partition is not a plain table partition": "other",
	"table public.audit_event: partition public.audit_event_p202611: c is a check, exclusion or foreign key constraint, a unique index or an index with an expression or a predicate of the partition alone": "other",

	// A name of the catalog with ": " in it can make the class more severe only: the partition "x: column a" holds
	// a trigger, and a constraint named "trigger t".
	"table public.audit_event: partition public.\"x: column a\": trigger evil is a trigger that the manifest does not name":                          "trigger",
	"table public.audit_event: partition public.\"x: rule r\": the partition grants a privilege on it or on a column to another role than its owner": "grant",
	"table public.audit_event: partition public.\"x: policy p\": trigger t is missing":                                                               "policy",
}
