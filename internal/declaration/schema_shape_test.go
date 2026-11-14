package declaration

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// shapeSchema is a schema of one definition doc whose permission p has the expression expr, after extra.
func shapeSchema(extra, expr string) string {
	return "definition user {}\n" + extra + "definition doc {\n\trelation own: user\n\trelation parent: doc\n\tpermission view = own\n\tpermission wide = " + expr + "\n}\n"
}

// joined gives n copies of term joined by op.
func joined(term, op string, n int) string {
	return strings.TrimSuffix(strings.Repeat(term+op, n), op)
}

// expectShapeFault expects err to be a schema fault at line that names the limit.
func expectShapeFault(t *testing.T, err error, line int, limit int) {
	t.Helper()
	var fault *SchemaError
	if !errors.As(err, &fault) {
		t.Fatalf("got %v, want a schema fault", err)
	}
	if fault.Line != line || !strings.Contains(fault.Message, fmt.Sprint(limit)) {
		t.Fatalf("got the fault %q at line %d, want one at line %d that names the limit %d", fault.Message, fault.Line, line, limit)
	}
}

func TestSchemaShape(t *testing.T) {
	t.Run("ST-DECL-INV88-T A schema beyond the shape bounds is refused before it compiles, and a hostile wide schema in well under 1 s", func(t *testing.T) {
		accepted := map[string]string{
			"256 terms joined by +":           shapeSchema("", joined("own", " + ", MaxPermissionTerms)),
			"256 terms of &, - and arrows":    shapeSchema("", "own & ("+joined("parent->view", " + ", MaxPermissionTerms-2)+") - own"),
			"256 terms over three lines":      shapeSchema("", joined("own", " +\n\t\t", MaxPermissionTerms)),
			"operators in comments and a CEL": shapeSchema("// "+joined("own", " + ", 400)+"\n/* "+joined("own", " & ", 400)+" */\ncaveat cav(n int) {\n\t"+"["+joined("n - 1", ", ", 300)+"].size() > 0 && {'k': '}'}.size() > 0\n}\n", "own"),
			"4096 relations and permissions":  "definition user {}\ndefinition doc {\n" + relations(MaxSchemaMembers) + "}\n",
			// The legal worst cases of the review of PR #302 that compile in well under a second.
			"a triple-quoted string with a brace in a caveat body": tripleQuoted + shapeSchema("", joined("own", " + ", MaxPermissionTerms)),
			"64 parentheses open":                  shapeSchema("", nested(MaxSchemaNesting)),
			"64 parentheses open in a caveat body": "caveat cav(n int) {\n\t" + strings.Repeat("(", MaxSchemaNesting) + "n > 0" + strings.Repeat(")", MaxSchemaNesting) + "\n}\n" + shapeSchema("", "own"),
			"exclusions nested 64 deep":            shapeSchema("", exclusions(MaxSchemaNesting/2)),
		}
		for name, source := range accepted {
			t.Run(name, func(t *testing.T) {
				if _, err := CompileAuthzSchema("authz/schema.zed", []byte(source)); err != nil {
					t.Fatalf("the schema is refused: %v", err)
				}
			})
		}
		t.Run("257 terms", func(t *testing.T) {
			_, err := CompileAuthzSchema("authz/schema.zed", []byte(shapeSchema("", joined("own", " + ", MaxPermissionTerms+1))))
			expectShapeFault(t, err, 6, MaxPermissionTerms)
		})
		t.Run("257 terms of & and -", func(t *testing.T) {
			_, err := CompileAuthzSchema("authz/schema.zed", []byte(shapeSchema("", joined("own", " & ", 200)+" - "+joined("parent->view", " - ", 57))))
			expectShapeFault(t, err, 6, MaxPermissionTerms)
		})
		// Text that is no comment and no caveat body hides no operator: a stray quote, a brace before the
		// definition, and a definition named caveat.
		hiding := map[string]string{
			"after a string": shapeSchema("", "own ' & ' & "+joined("own", " & ", MaxPermissionTerms+1)),
			"after a relation of type caveat and a brace": "definition caveat {}\ndefinition doc {\n\trelation own: caveat\n}\ndefinition other {\n\trelation own: user\n\tpermission wide = " + joined("own", " & ", MaxPermissionTerms+1) + "\n}\n",
			// Review of PR #302, M1: a triple-quoted CEL string that holds a brace ends where the lexer ends it.
			"after a triple-quoted string with a brace in a caveat body": tripleQuoted + shapeSchema("", joined("own", " + ", 20000)),
			"after a brace before it":                                    "}\n" + shapeSchema("", joined("own", " & ", MaxPermissionTerms+1)),
			"in a definition named caveat":                               "definition caveat {\n\trelation own: caveat\n\tpermission wide = " + joined("own", " & ", MaxPermissionTerms+1) + "\n}\n",
		}
		for name, source := range hiding {
			t.Run(name, func(t *testing.T) {
				_, err := CompileAuthzSchema("authz/schema.zed", []byte(source))
				var fault *SchemaError
				if !errors.As(err, &fault) || !strings.Contains(fault.Message, fmt.Sprint(MaxPermissionTerms)) {
					t.Fatalf("got %v, want a fault that names the limit %d", err, MaxPermissionTerms)
				}
			})
		}
		t.Run("4097 relations and permissions after a triple-quoted string with a brace in a caveat body", func(t *testing.T) {
			_, err := CompileAuthzSchema("authz/schema.zed", []byte(tripleQuoted+"definition user {}\ndefinition doc {\n"+relations(6000)+"}\n"))
			expectShapeFault(t, err, 5+2+MaxSchemaMembers+1, MaxSchemaMembers)
		})
		// Review of PR #302, M2: the parser recurses once for each parenthesis, and about 524000 of them end the
		// process with a stack overflow.
		t.Run("65 parentheses open", func(t *testing.T) {
			_, err := CompileAuthzSchema("authz/schema.zed", []byte(shapeSchema("", nested(MaxSchemaNesting+1))))
			expectShapeFault(t, err, 6, MaxSchemaNesting)
		})
		t.Run("65 parentheses open in a caveat body", func(t *testing.T) {
			_, err := CompileAuthzSchema("authz/schema.zed", []byte("caveat cav(n int) {\n\t"+strings.Repeat("(", MaxSchemaNesting+1)+"n > 0"+strings.Repeat(")", MaxSchemaNesting+1)+"\n}\n"+shapeSchema("", "own")))
			expectShapeFault(t, err, 2, MaxSchemaNesting)
		})
		t.Run("parentheses of 1 MiB", func(t *testing.T) {
			depth := (MaxSchemaBytes - 200) / 2
			start := time.Now()
			_, err := CompileAuthzSchema("authz/schema.zed", []byte(shapeSchema("", strings.Repeat("(", depth)+"own"+strings.Repeat(")", depth))))
			expectShapeFault(t, err, 6, MaxSchemaNesting)
			if elapsed := time.Since(start); elapsed > time.Second/4 {
				t.Fatalf("the refusal took %v, want well under 1 s", elapsed)
			}
		})
		t.Run("4097 relations and permissions", func(t *testing.T) {
			_, err := CompileAuthzSchema("authz/schema.zed", []byte("definition user {}\ndefinition doc {\n"+relations(MaxSchemaMembers+1)+"}\n"))
			expectShapeFault(t, err, 2+MaxSchemaMembers+1, MaxSchemaMembers)
		})
		t.Run("a hostile wide intersection of 1 MiB", func(t *testing.T) {
			expr := joined("own", " & ", (MaxSchemaBytes-200)/6)
			source := shapeSchema("", expr)
			if len(source) > MaxSchemaBytes {
				t.Fatalf("the schema has %d bytes, more than MaxSchemaBytes", len(source))
			}
			start := time.Now()
			_, err := CompileAuthzSchema("authz/schema.zed", []byte(source))
			elapsed := time.Since(start)
			expectShapeFault(t, err, 6, MaxPermissionTerms)
			if elapsed > time.Second/4 {
				t.Fatalf("the refusal took %v, want well under 1 s", elapsed)
			}
			t.Logf("refused in %v", elapsed)
		})
		// The legal worst cases of the review of PR #302 at about 1 MiB pass the shape check at once; the compiler
		// then takes seconds on them, within the time of a check.
		t.Run("legal worst cases of 1 MiB pass the shape check", func(t *testing.T) {
			for name, member := range map[string]string{
				"permissions of 256 terms of +": joined("own", "+", MaxPermissionTerms),
				"permissions of 256 terms of &": joined("own", "&", MaxPermissionTerms),
				"exclusions nested 64 deep":     exclusions(MaxSchemaNesting / 2),
			} {
				var b strings.Builder
				b.WriteString("definition user {}\ndefinition doc {\n\trelation own: user\n")
				for i := 0; b.Len() < MaxSchemaBytes-2000 && i < MaxSchemaMembers-1; i++ {
					fmt.Fprintf(&b, "\tpermission p%d = %s\n", i, member)
				}
				b.WriteString("}\n")
				start := time.Now()
				if err := checkSchemaShape("authz/schema.zed", []byte(b.String())); err != nil {
					t.Fatalf("%s: the shape check refuses %d bytes: %v", name, b.Len(), err)
				}
				// The time is logged, not bounded: the lexer sends each token on a channel, which the race detector of
				// a shared CI runner makes more than ten times slower. The bounds on time are on the refusals.
				t.Logf("%s: %d bytes pass the shape check in %v", name, b.Len(), time.Since(start))
			}
		})
		t.Run("through checking", func(t *testing.T) {
			service, schema := td03Files(t)
			wide := change(t, schema, "\tpermission edit = owner\n", "\tpermission edit = owner\n\tpermission wide = "+joined("owner", " & ", MaxPermissionTerms+1)+"\n")
			fault := expectOneFault(t, checkTD03(t, service, wide), "schema-compile", "authz/schema.zed")
			if !strings.Contains(fault.Message, fmt.Sprint(MaxPermissionTerms)) || !strings.Contains(fault.Message, "wide") {
				t.Fatalf("the fault does not name the permission and the limit: %+v", fault)
			}
		})
	})
}

// tripleQuoted is a caveat whose body is a CEL comparison with a triple-quoted string over three lines that holds
// an open brace (review of PR #302, M1).
const tripleQuoted = "caveat c(s string) {\n\ts == \"\"\"\n{\n\"\"\"\n}\n"

// nested gives the relation own in depth parentheses.
func nested(depth int) string {
	return strings.Repeat("(", depth) + "own" + strings.Repeat(")", depth)
}

// exclusions gives n exclusions nested in pairs of parentheses, 2n deep: (own - (own - (... own))).
func exclusions(n int) string {
	expr := "own"
	for range n {
		expr = "(own - (" + expr + "))"
	}
	return expr
}

// relations gives n members of a definition, relations and permissions in turn, one a line.
func relations(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		if i%2 == 0 {
			fmt.Fprintf(&b, "\trelation rel%d: user\n", i)
		} else {
			fmt.Fprintf(&b, "\tpermission perm%d = rel%d\n", i, i-1)
		}
	}
	return b.String()
}
