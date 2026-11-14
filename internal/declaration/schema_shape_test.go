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
			"operators in comments and a CEL": shapeSchema("// "+joined("own", " + ", 400)+"\n/* "+joined("own", " & ", 400)+" */\ncaveat cav(n int) {\n\t"+"["+joined("n - 1", ", ", 200)+"].size() > 0 && {'k': '}'}.size() > 0\n}\n", "own"),
			"4096 relations and permissions":  "definition user {}\ndefinition doc {\n" + relations(MaxSchemaMembers) + "}\n",
			// The legal worst cases of the review of PR #302 that compile in well under a second.
			"a triple-quoted string with a brace in a caveat body": tripleQuoted + shapeSchema("", joined("own", " + ", MaxPermissionTerms)),
			"64 parentheses open":                  shapeSchema("", nested(MaxSchemaNesting)),
			"24 parentheses open in a caveat body": "caveat cav(n int) {\n\t" + strings.Repeat("(", MaxCaveatBodyNesting) + "n > 0" + strings.Repeat(")", MaxCaveatBodyNesting) + "\n}\n" + shapeSchema("", "own"),
			"exclusions nested 64 deep":            shapeSchema("", exclusions(MaxSchemaNesting/2)),
			// Review of PR #302, M3 and L2: the bounds on caveats.
			"a caveat parameter type 8 deep":               caveatOfType("list", MaxCaveatTypeNesting) + shapeSchema("", "own"),
			"a map of a list of a map, and types after it": "caveat cav(a map<list<map<int>>>, b list<list<list<list<list<list<list<list<int>>>>>>>>, c map<string>) {\n\ta.size() > 0 && b.size() > 0 && c.size() > 0\n}\n" + shapeSchema("", "own"),
			"64 caveats":                     caveats(MaxCaveats, 4) + shapeSchema("", "own"),
			"caveat bodies of 64 KiB in all": caveats(MaxCaveatBodyBytes/MaxCaveatBodyBytesEach, MaxCaveatBodyBytesEach) + shapeSchema("", "own"),
			// Review of PR #302, M6: the bounds on each caveat body.
			"a caveat body of 4 KiB":                         caveats(1, MaxCaveatBodyBytesEach) + shapeSchema("", "own"),
			"a caveat body of 256 indexes and operators":     indexCompares(64, " || true") + shapeSchema("", "own"),
			"256 calls and operators in each of two caveats": sizeCalls(0, 85, " || true || true") + sizeCalls(1, 85, " || true || true") + shapeSchema("", "own"),
			// Review of PR #302, M4: the bounds on comprehension macros and list literals in caveat bodies.
			"16 comprehension macros in two caveats": chainedMaps(0, MaxCaveatMacros/2) + chainedMaps(1, MaxCaveatMacros/2) + shapeSchema("", "own"),
			"each kind of comprehension macro":       "caveat cav(xs list<int>) {\n\txs.map(x, x + 1).filter(x, x > 1).all(x, x > 0) && xs.exists(x, x > 2) && xs.exists_one(x, x == 3)\n}\n" + shapeSchema("", "own"),
			"names of macros that are no macro call": "caveat cav(m map<int>, map int) {\n\tm.map > 0 && m.filter > 0 && map > 0 && " + strings.Repeat("m.exists + ", 20) + "0 > 0\n}\n" + shapeSchema("", "own"),
			"a list literal 24 deep":                 listLiteral(MaxCaveatBodyNesting) + shapeSchema("", "own"),
			"lists 24 deep one after the other":      "caveat cav(n int) {\n\t" + strings.Repeat(strings.Repeat("[", MaxCaveatBodyNesting)+"n"+strings.Repeat("]", MaxCaveatBodyNesting)+".size() > 0 && ", 4) + "n > 0\n}\n" + shapeSchema("", "own"),
			// Review of PR #302, M5: brackets, braces and parentheses count together in a caveat body.
			"a map literal 24 deep":            mapLiteral(MaxCaveatBodyNesting) + shapeSchema("", "own"),
			"map literals 24 deep compared":    "caveat cav(n int) {\n\t" + nestedMaps(MaxCaveatBodyNesting) + " == " + nestedMaps(MaxCaveatBodyNesting) + "\n}\n" + shapeSchema("", "own"),
			"a mix of [, { and ( 24 deep":      mixedNesting(MaxCaveatBodyNesting) + shapeSchema("", "own"),
			"maps 24 deep one after the other": "caveat cav(n int) {\n\t" + strings.Repeat(nestedMaps(MaxCaveatBodyNesting)+".size() > 0 && ", 4) + "n > 0\n}\n" + shapeSchema("", "own"),
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
			expectShapeFault(t, err, 2, MaxCaveatBodyNesting)
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
		// Review of PR #302, M3: the CEL checker took 16 s on a list type 4000 deep, and ran out of memory at 150000.
		t.Run("a caveat parameter type 9 deep", func(t *testing.T) {
			_, err := CompileAuthzSchema("authz/schema.zed", []byte(caveatOfType("list", MaxCaveatTypeNesting+1)+shapeSchema("", "own")))
			expectShapeFault(t, err, 1, MaxCaveatTypeNesting)
		})
		t.Run("a map type 9 deep after a closed type", func(t *testing.T) {
			_, err := CompileAuthzSchema("authz/schema.zed", []byte("caveat cav(a list<list<int>>,\n\tb "+strings.Repeat("map<", MaxCaveatTypeNesting+1)+"int"+strings.Repeat(">", MaxCaveatTypeNesting+1)+") {\n\ta.size() > 0\n}\n"+shapeSchema("", "own")))
			expectShapeFault(t, err, 2, MaxCaveatTypeNesting)
		})
		t.Run("a caveat parameter type of 1 MiB", func(t *testing.T) {
			source := caveatOfType("list", (MaxSchemaBytes-300)/6) + shapeSchema("", "own")
			if len(source) > MaxSchemaBytes {
				t.Fatalf("the schema has %d bytes, more than MaxSchemaBytes", len(source))
			}
			start := time.Now()
			_, err := CompileAuthzSchema("authz/schema.zed", []byte(source))
			expectShapeFault(t, err, 1, MaxCaveatTypeNesting)
			if elapsed := time.Since(start); elapsed > time.Second/4 {
				t.Fatalf("the refusal took %v, want well under 1 s", elapsed)
			}
		})
		// Review of PR #302, L2: the CEL compiler took about 0.9 s for each caveat body of 95 KB.
		t.Run("65 caveats", func(t *testing.T) {
			_, err := CompileAuthzSchema("authz/schema.zed", []byte(caveats(MaxCaveats+1, 4)+shapeSchema("", "own")))
			expectShapeFault(t, err, 3*MaxCaveats+1, MaxCaveats)
		})
		t.Run("caveat bodies of 64 KiB and 4 KiB in all", func(t *testing.T) {
			source := caveats(MaxCaveatBodyBytes/MaxCaveatBodyBytesEach+1, MaxCaveatBodyBytesEach) + shapeSchema("", "own")
			_, err := CompileAuthzSchema("authz/schema.zed", []byte(source))
			expectShapeFault(t, err, 3*(MaxCaveatBodyBytes/MaxCaveatBodyBytesEach+1), MaxCaveatBodyBytes)
		})
		t.Run("caveat bodies of 64 KiB and a body with no end", func(t *testing.T) {
			source := shapeSchema("", "own") + caveats(MaxCaveatBodyBytes/MaxCaveatBodyBytesEach, MaxCaveatBodyBytesEach) + strings.TrimSuffix(caveats(1, 100), "}\n")
			_, err := CompileAuthzSchema("authz/schema.zed", []byte(source))
			var fault *SchemaError
			if !errors.As(err, &fault) || !strings.Contains(fault.Message, fmt.Sprint(MaxCaveatBodyBytes)) {
				t.Fatalf("got %v, want a fault that names the limit %d", err, MaxCaveatBodyBytes)
			}
		})
		// Review of PR #302, M6: the CEL checker took 53 s on 4000 of m['a'] != m['b'] in one body of 64 KB.
		t.Run("a caveat body of 4 KiB and one byte", func(t *testing.T) {
			_, err := CompileAuthzSchema("authz/schema.zed", []byte(caveats(1, MaxCaveatBodyBytesEach+1)+shapeSchema("", "own")))
			expectShapeFault(t, err, 3, MaxCaveatBodyBytesEach)
		})
		t.Run("a caveat body of 4 KiB and one byte with no end", func(t *testing.T) {
			_, err := CompileAuthzSchema("authz/schema.zed", []byte(shapeSchema("", "own")+strings.TrimSuffix(caveats(1, MaxCaveatBodyBytesEach+1), "}\n")))
			var fault *SchemaError
			if !errors.As(err, &fault) || !strings.Contains(fault.Message, fmt.Sprint(MaxCaveatBodyBytesEach)) {
				t.Fatalf("got %v, want a fault that names the limit %d", err, MaxCaveatBodyBytesEach)
			}
		})
		t.Run("a caveat body of 257 indexes and operators", func(t *testing.T) {
			_, err := CompileAuthzSchema("authz/schema.zed", []byte(indexCompares(64, " || true || true")+shapeSchema("", "own")))
			expectShapeFault(t, err, 2, MaxCaveatBodyCalls)
		})
		t.Run("a caveat body of 257 calls and operators", func(t *testing.T) {
			_, err := CompileAuthzSchema("authz/schema.zed", []byte(sizeCalls(0, 86, "")+shapeSchema("", "own")))
			expectShapeFault(t, err, 2, MaxCaveatBodyCalls)
		})
		t.Run("a caveat body of 257 operators of every kind", func(t *testing.T) {
			_, err := CompileAuthzSchema("authz/schema.zed", []byte("caveat cav(n int, m map<int>) {\n\t"+strings.Repeat("(n + n - n * n / 1 % 2 == 0 != false) && !(n < 1 || n > 2 || n <= 3 || n >= 4) && ('a' in m ? true : false) && ", 13)+"true\n}\n"+shapeSchema("", "own")))
			expectShapeFault(t, err, 2, MaxCaveatBodyCalls)
		})
		t.Run("4000 of m['a'] != m['b'] in one body of 64 KB", func(t *testing.T) {
			start := time.Now()
			_, err := CompileAuthzSchema("authz/schema.zed", []byte(indexCompares(4000, "")+shapeSchema("", "own")))
			expectShapeFault(t, err, 2, MaxCaveatBodyCalls)
			if elapsed := time.Since(start); elapsed > time.Second/4 {
				t.Fatalf("the refusal took %v, want well under 1 s", elapsed)
			}
		})
		t.Run("ten caveat bodies of 95 KB", func(t *testing.T) {
			source := caveats(10, 95000) + shapeSchema("", "own")
			start := time.Now()
			_, err := CompileAuthzSchema("authz/schema.zed", []byte(source))
			expectShapeFault(t, err, 2, MaxCaveatBodyBytesEach)
			if elapsed := time.Since(start); elapsed > time.Second/4 {
				t.Fatalf("the refusal took %v, want well under 1 s", elapsed)
			}
		})
		// Review of PR #302, M4: each comprehension macro can wrap the inferred type once more, and the CEL checker took
		// 1 min 48 s on 15 caveats of 240 chained maps each, a schema of 62 KB in every other bound.
		t.Run("17 chained maps", func(t *testing.T) {
			_, err := CompileAuthzSchema("authz/schema.zed", []byte(chainedMaps(0, MaxCaveatMacros+1)+shapeSchema("", "own")))
			expectShapeFault(t, err, 2, MaxCaveatMacros)
		})
		t.Run("17 comprehension macros in two caveats", func(t *testing.T) {
			_, err := CompileAuthzSchema("authz/schema.zed", []byte(chainedMaps(0, 9)+chainedMaps(1, 8)+shapeSchema("", "own")))
			expectShapeFault(t, err, 5, MaxCaveatMacros)
		})
		t.Run("17 macros of other kinds after a space", func(t *testing.T) {
			_, err := CompileAuthzSchema("authz/schema.zed", []byte("caveat cav(xs list<int>) {\n\txs"+strings.Repeat(".filter (x, x > 0)", 8)+strings.Repeat(".exists_one(x, x > 0) && xs", 4)+strings.Repeat(".exists(x, x > 0) && xs", 4)+".all(x, x > 0)\n}\n"+shapeSchema("", "own")))
			expectShapeFault(t, err, 2, MaxCaveatMacros)
		})
		t.Run("a list literal 25 deep", func(t *testing.T) {
			_, err := CompileAuthzSchema("authz/schema.zed", []byte(listLiteral(MaxCaveatBodyNesting+1)+shapeSchema("", "own")))
			expectShapeFault(t, err, 2, MaxCaveatBodyNesting)
		})
		// Review of PR #302, M5: nested map literals gave the same slow type formatting; 46 caveats of maps 245 deep,
		// 58 KB in every other bound, took 2 min to check.
		t.Run("a map literal 25 deep", func(t *testing.T) {
			_, err := CompileAuthzSchema("authz/schema.zed", []byte(mapLiteral(MaxCaveatBodyNesting+1)+shapeSchema("", "own")))
			expectShapeFault(t, err, 2, MaxCaveatBodyNesting)
		})
		t.Run("a map literal with string keys 25 deep", func(t *testing.T) {
			_, err := CompileAuthzSchema("authz/schema.zed", []byte("caveat cav(n int) {\n\t"+strings.Repeat("{'k': ", MaxCaveatBodyNesting+1)+"n"+strings.Repeat("}", MaxCaveatBodyNesting+1)+".size() > 0\n}\n"+shapeSchema("", "own")))
			expectShapeFault(t, err, 2, MaxCaveatBodyNesting)
		})
		t.Run("a mix of [, { and ( 25 deep", func(t *testing.T) {
			_, err := CompileAuthzSchema("authz/schema.zed", []byte(mixedNesting(MaxCaveatBodyNesting+1)+shapeSchema("", "own")))
			expectShapeFault(t, err, 2, MaxCaveatBodyNesting)
		})
		t.Run("map literals 240 deep compared", func(t *testing.T) {
			start := time.Now()
			_, err := CompileAuthzSchema("authz/schema.zed", []byte("caveat cav(n int) {\n\t"+nestedMaps(240)+" == "+nestedMaps(240)+"\n}\n"+shapeSchema("", "own")))
			expectShapeFault(t, err, 2, MaxCaveatBodyNesting)
			if elapsed := time.Since(start); elapsed > time.Second/4 {
				t.Fatalf("the refusal took %v, want well under 1 s", elapsed)
			}
		})
		t.Run("46 caveats of map literals 245 deep", func(t *testing.T) {
			var b strings.Builder
			for i := range 46 {
				fmt.Fprintf(&b, "caveat cav%d(n int) {\n\t%s.size() > 0\n}\n", i, nestedMaps(245))
			}
			start := time.Now()
			_, err := CompileAuthzSchema("authz/schema.zed", []byte(b.String()+shapeSchema("", "own")))
			expectShapeFault(t, err, 2, MaxCaveatBodyNesting)
			if elapsed := time.Since(start); elapsed > time.Second/4 {
				t.Fatalf("the refusal took %v, want well under 1 s", elapsed)
			}
		})
		t.Run("15 caveats of 240 chained maps", func(t *testing.T) {
			var b strings.Builder
			for i := range 15 {
				b.WriteString(chainedMaps(i, 240))
			}
			start := time.Now()
			_, err := CompileAuthzSchema("authz/schema.zed", []byte(b.String()+shapeSchema("", "own")))
			expectShapeFault(t, err, 2, MaxCaveatMacros)
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

// caveatOfType gives a caveat with one parameter of the type kind nested depth deep, such as list<list<int>>.
func caveatOfType(kind string, depth int) string {
	return "caveat cav(a " + strings.Repeat(kind+"<", depth) + "int" + strings.Repeat(">", depth) + ") {\n\ta.size() > 0\n}\n"
}

// caveats gives n caveats of three lines each, whose bodies have size bytes between their braces: a CEL comparison
// and spaces. A size of less than 9 gives the comparison alone.
func caveats(n, size int) string {
	var b strings.Builder
	for i := range n {
		expr := "\n\tn > 0"
		fmt.Fprintf(&b, "caveat cav%d(n int) {%s%s\n}\n", i, expr, strings.Repeat(" ", max(size-len(expr)-1, 0)))
	}
	return b.String()
}

// chainedMaps gives the caveat cav<i> of three lines whose body chains n maps, each of which wraps the inferred type
// in a list once more: [1].map(x0, [x0]).map(x1, [x1]) (review of PR #302, M4).
func chainedMaps(i, n int) string {
	var b strings.Builder
	b.WriteString("[1]")
	for j := range n {
		fmt.Fprintf(&b, ".map(x%d, [x%d])", j, j)
	}
	return fmt.Sprintf("caveat cav%d(n int) {\n\t%s.size() > n\n}\n", i, b.String())
}

// listLiteral gives a caveat of three lines whose body has a list literal depth deep.
func listLiteral(depth int) string {
	return "caveat cav(n int) {\n\t" + strings.Repeat("[", depth) + "n" + strings.Repeat("]", depth) + ".size() > 0\n}\n"
}

// indexCompares gives a caveat of three lines whose body joins n of m['a'] != m['b'] with &&, four calls and
// operators each but the last, and then tail: 4n - 1 in all with no tail (review of PR #302, M6).
func indexCompares(n int, tail string) string {
	return "caveat cav(m map<int>) {\n\t" + joined("m['a'] != m['b']", " && ", n) + tail + "\n}\n"
}

// sizeCalls gives the caveat cav<i> of three lines whose body joins n of p.size() > 0 with &&, three calls and
// operators each but the last, and then tail: 3n - 1 in all with no tail (review of PR #302, M6).
func sizeCalls(i, n int, tail string) string {
	return fmt.Sprintf("caveat cav%d(p list<int>) {\n\t%s%s\n}\n", i, joined("p.size() > 0", " && ", n), tail)
}

// nestedMaps gives a map literal depth deep, {1: {1: n}} for a depth of 2 (review of PR #302, M5).
func nestedMaps(depth int) string {
	return strings.Repeat("{1: ", depth) + "n" + strings.Repeat("}", depth)
}

// mapLiteral gives a caveat of three lines whose body has a map literal depth deep.
func mapLiteral(depth int) string {
	return "caveat cav(n int) {\n\t" + nestedMaps(depth) + ".size() > 0\n}\n"
}

// mixedNesting gives a caveat of three lines whose body opens [, { and ( in turn, depth in all, as in
// [{1: ([{1: (n)}])}].
func mixedNesting(depth int) string {
	var open, closed strings.Builder
	for i := range depth {
		switch i % 3 {
		case 0:
			open.WriteString("[")
			closed.WriteString("]")
		case 1:
			open.WriteString("{1: ")
			closed.WriteString("}")
		default:
			open.WriteString("(")
			closed.WriteString(")")
		}
	}
	c := []rune(closed.String())
	for i, j := 0, len(c)-1; i < j; i, j = i+1, j-1 {
		c[i], c[j] = c[j], c[i]
	}
	return "caveat cav(n int) {\n\t" + open.String() + "n" + string(c) + ".size() > 0\n}\n"
}

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
