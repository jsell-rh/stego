package declaration

import (
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	core "github.com/authzed/spicedb/pkg/proto/core/v1"
)

func TestTenantProvisioning(t *testing.T) {
	const inv78 = "ST-DECL-INV78-T A pooled service without its tenant provisioning, or with a public or unknown permission, is refused"
	const line = "  provisioning: {resource_type: service, provision: provision, retire: retire}\n"
	service, schema := td03Files(t)
	t.Run(inv78+": TD03 with the tenant provisioning on service with provision and retire is accepted", func(t *testing.T) {
		expectAccepted(t, checkTD03(t, service, schema))
	})
	for _, c := range []struct {
		name, provisioning, path string
		texts                    []string
	}{
		{"no tenant provisioning", "", "tenancy.provisioning", []string{"tenant provisioning", "add tenancy.provisioning"}},
		{"no Retire permission", "  provisioning: {resource_type: service, provision: provision}\n", "tenancy.provisioning.retire", []string{"tenant provisioning", "Retire permission"}},
		{"the Provision permission approve, which the schema lacks", "  provisioning: {resource_type: service, provision: approve, retire: retire}\n", "tenancy.provisioning.provision", []string{"tenant provisioning", "approve", "lacks"}},
		{"the Provision permission declared public", "  provisioning: {resource_type: service, provision: public, retire: retire}\n", "tenancy.provisioning.provision", []string{"tenant provisioning", "public"}},
		{"a resource type that the schema lacks", "  provisioning: {resource_type: tenant_admin, provision: provision, retire: retire}\n", "tenancy.provisioning.resource_type", []string{"tenant provisioning", "tenant_admin", "lacks"}},
		{"the resource type document of the entity Todo", "  provisioning: {resource_type: document, provision: edit, retire: edit}\n", "tenancy.provisioning.resource_type", []string{"tenant provisioning", "document", "of its own"}},
		{"the subject type user", "  provisioning: {resource_type: user, provision: provision, retire: retire}\n", "tenancy.provisioning.resource_type", []string{"tenant provisioning", "user", "subject type"}},
	} {
		t.Run(inv78+": "+c.name, func(t *testing.T) {
			faults := checkTD03(t, change(t, service, line, c.provisioning), schema)
			if c.name == "the subject type user" {
				// The type user has no permissions, so the two permissions are also lacking; the type is the fault here.
				var kept []ValidationError
				for _, fault := range faults {
					if fault.Path == c.path {
						kept = append(kept, fault)
					}
				}
				faults = kept
			}
			fault := expectOneFault(t, faults, "tenant-provisioning", c.texts...)
			if fault.Path != c.path {
				t.Errorf("want the fault at %s, got %+v", c.path, fault)
			}
		})
	}
	// The two schemas of the security review of PR 253 (M1): a permission that reaches a wildcard is public in
	// effect, also through a union, a userset or an arrow.
	for name, definition := range map[string]string{
		"a relation of user:* only":               "definition service {\n\trelation anyone: user:*\n\tpermission provision = anyone\n\tpermission retire = anyone\n}\n",
		"a relation of user or user:*":            "definition service {\n\trelation operator: user | user:*\n\tpermission provision = operator\n\tpermission retire = operator\n}\n",
		"a userset of a group that admits user:*": "definition crew {\n\trelation member: user:*\n}\n\ndefinition service {\n\trelation operator: crew#member\n\tpermission provision = operator\n\tpermission retire = operator\n}\n",
		"an arrow to a parent that admits user:*": "definition org {\n\trelation admin: user:*\n\tpermission manage = admin\n}\n\ndefinition service {\n\trelation parent: org\n\tpermission provision = parent->manage\n\tpermission retire = parent->manage\n}\n",
	} {
		t.Run(inv78+": a permission that reaches a wildcard, "+name, func(t *testing.T) {
			widened := strings.Replace(schema, "definition service {\n\trelation operator: user\n\tpermission provision = operator\n\tpermission retire = operator\n}\n", definition, 1)
			if widened == schema {
				t.Fatal("the schema of TD03 holds no definition service")
			}
			var wildcards []ValidationError
			for _, fault := range checkTD03(t, service, widened) {
				if fault.Rule == "tenant-provisioning" && strings.Contains(fault.Message, "wildcard") {
					wildcards = append(wildcards, fault)
				}
			}
			if len(wildcards) != 2 || wildcards[0].Path != "tenancy.provisioning.provision" || wildcards[1].Path != "tenancy.provisioning.retire" {
				t.Errorf("want a wildcard fault for provision and for retire, got %+v", wildcards)
			}
		})
	}
	t.Run(inv78+": a wildcard that an exclusion takes away, or that only one side of an intersection reaches, is not public", func(t *testing.T) {
		definition := "definition service {\n\trelation operator: user\n\trelation anyone: user:*\n\trelation banned: user:*\n\tpermission provision = operator & anyone\n\tpermission retire = operator - banned\n}\n"
		widened := strings.Replace(schema, "definition service {\n\trelation operator: user\n\tpermission provision = operator\n\tpermission retire = operator\n}\n", definition, 1)
		expectAccepted(t, checkTD03(t, service, widened))
	})
	t.Run(inv78+": a pooled service with a provisioning and no authorization is refused, and a single tenancy declares none", func(t *testing.T) {
		unbound := withoutAuthorizationParts(t, service)
		expectOneFault(t, Validate(readTD03(t, unbound)).All(), "tenant-provisioning", "needs authorization")
		single := change(t, service, "tenancy:\n  kind: pooled\n  key: tenant_id\n  claim: tenant\n", "tenancy:\n  kind: single\n  reason: one customer per deployment\n")
		expectOneFault(t, Validate(readTD03(t, single)).All(), "tenant-provisioning", "single", "remove tenancy.provisioning")
	})

	// The cases of the recheck of the security review of PR 253: whether service#provision reaches a wildcard.
	for _, c := range []struct {
		name, schema string
		want         bool
	}{
		{"a cycle of relations with a wildcard in it", "definition user {}\ndefinition service {\n relation aaa: service#bbb | user\n relation bbb: service#aaa | user:*\n permission provision = aaa\n}\n", true},
		{"a cycle of permissions with no wildcard", "definition user {}\ndefinition service {\n relation own: user\n permission provision = qqq + own\n permission qqq = provision\n}\n", false},
		{"an arrow to a permission that reaches a wildcard", "definition user {}\ndefinition org {\n relation mem: user:*\n permission admin = mem\n}\ndefinition service {\n relation org: org\n permission provision = org->admin\n}\n", true},
		{"a wildcard of a type that is not user", "definition user {}\ndefinition bot {}\ndefinition service {\n relation own: bot:*\n permission provision = own\n}\n", true},
		{"a wildcard with a caveat", "caveat c(x int) { x > 0 }\ndefinition user {}\ndefinition service {\n relation own: user:* with c\n permission provision = own\n}\n", true},
		{"a wildcard with an expiration", "use expiration\ndefinition user {}\ndefinition service {\n relation own: user:* with expiration\n permission provision = own\n}\n", true},
		{"a permission through two permissions to a wildcard", "definition user {}\ndefinition service {\n relation wild: user:*\n permission inner = wild\n permission mid = inner\n permission provision = mid\n}\n", true},
		{"a userset that admits a wildcard", "definition user {}\ndefinition group {\n relation member: user:*\n}\ndefinition service {\n relation own: group#member\n permission provision = own\n}\n", true},
		{"an intersection with one wildcard", "definition user {}\ndefinition service {\n relation own: user\n relation wild: user:*\n permission provision = own & wild\n}\n", false},
		{"an intersection of two wildcards", "definition user {}\ndefinition service {\n relation wild: user:*\n relation wild2: user:*\n permission provision = wild & wild2\n}\n", true},
		{"an exclusion whose base is a wildcard", "definition user {}\ndefinition service {\n relation own: user\n relation wild: user:*\n permission provision = wild - own\n}\n", true},
		{"an exclusion that only subtracts a wildcard", "definition user {}\ndefinition service {\n relation own: user\n relation wild: user:*\n permission provision = own - wild\n}\n", false},
		{"an intersection with a nested union", "definition user {}\ndefinition service {\n relation own: user\n relation wild: user:*\n permission provision = own & (wild + own) \n}\n", false},
		{"an all arrow to a permission that reaches a wildcard", "definition user {}\ndefinition org {\n relation mem: user:*\n permission admin = mem\n}\ndefinition service {\n relation org: org\n permission provision = org.all(admin)\n}\n", true},
	} {
		t.Run(inv78+": the recheck case "+c.name, func(t *testing.T) {
			compiled := compileTD03(t, c.schema)
			if got := compiled.reachesWildcard("service", "provision"); got != c.want {
				t.Errorf("want %v, got %v", c.want, got)
			}
		})
	}

	// A chain of diamonds made each level double the work of a search that does not keep its answers (security
	// review of PR 253, L3). The fixed point answers it at once, so a crafted schema cannot hang checking.
	t.Run(inv78+": a chain of 60 diamonds is checked in well under a second", func(t *testing.T) {
		chain := func(last string) string {
			var text strings.Builder
			fmt.Fprintf(&text, "definition user {}\ndefinition node60 {\n relation anyone: %s\n permission perm = anyone\n}\n", last)
			for i := 59; i >= 0; i-- {
				fmt.Fprintf(&text, "definition node%d {\n relation left: node%d\n relation right: node%d\n permission perm = left->perm + right->perm\n}\n", i, i+1, i+1)
			}
			return text.String()
		}
		// With no wildcard at the end, a search that does not keep its answers walks 2^60 paths.
		for last, want := range map[string]bool{"user": false, "user:*": true} {
			start := time.Now()
			if got := compileTD03(t, chain(last)).reachesWildcard("node0", "perm"); got != want {
				t.Errorf("a chain that ends in %s: want %v, got %v", last, want, got)
			}
			if elapsed := time.Since(start); elapsed > time.Second {
				t.Errorf("a chain that ends in %s took %v", last, elapsed)
			}
		}
	})
	// A long chain of arrows made the fixed point quadratic: each pass over the schema set one more node, so 12000
	// links took about 48 s (security review of PR 253, L4). The worklist sets each node once, when a node it reads
	// changes, so the time grows with the size of the schema.
	t.Run(inv78+": a chain of 12000 arrows is checked in linear time", func(t *testing.T) {
		const links = 12000
		var text strings.Builder
		fmt.Fprintf(&text, "definition user {}\ndefinition node%d {\n relation anyone: user:*\n permission perm = anyone\n}\n", links)
		for i := links - 1; i >= 0; i-- {
			fmt.Fprintf(&text, "definition node%d {\n relation next: node%d\n permission perm = next->perm\n}\n", i, i+1)
		}
		compiled := compileTD03(t, text.String())
		start := time.Now()
		reach := compiled.wildcardReach()
		elapsed := time.Since(start)
		if !reach["node0#perm"] || reach["node0#next"] {
			t.Errorf("node0#perm reaches a wildcard %v and node0#next %v, want true and false", reach["node0#perm"], reach["node0#next"])
		}
		if elapsed > 2*time.Second {
			t.Errorf("the wildcard reach of a chain of %d arrows took %v, want well under 2 s", links, elapsed)
		}
	})
	// A wide intersection whose parts repeat made the worklist quadratic in the fan-in of a node: it kept each
	// repeated edge and decided the whole intersection again at each one, so a 120 KB schema took 16.6 s (security
	// review of PR 294, M1). On the AND/OR graph each part is one input and each edge is followed once.
	for _, c := range []struct {
		name string
		text func() string
		node string
		want bool
	}{
		{"10000 repeats of one part and a last part with no wildcard", func() string {
			var b strings.Builder
			b.WriteString("definition user {}\ndefinition doc {\n relation aaa: user:*\n relation bbb: user\n permission xxx = ")
			b.WriteString(strings.Repeat("aaa & ", 10000))
			b.WriteString("bbb\n}\n")
			return b.String()
		}, "doc#xxx", false},
		{"10000 repeats of one part with a wildcard", func() string {
			return "definition user {}\ndefinition doc {\n relation aaa: user:*\n permission xxx = " + strings.Repeat("aaa & ", 9999) + "aaa\n}\n"
		}, "doc#xxx", true},
		{"5000 distinct parts, each a wildcard", func() string {
			var b strings.Builder
			b.WriteString("definition user {}\ndefinition doc {\n")
			for i := range 5000 {
				fmt.Fprintf(&b, " relation rel%d: user:*\n", i)
			}
			b.WriteString(" permission xxx = ")
			for i := range 5000 {
				fmt.Fprintf(&b, "rel%d & ", i)
			}
			b.WriteString("rel0\n}\n")
			return b.String()
		}, "doc#xxx", true},
		{"an arrow over 3000 subject types, repeated 100 times", func() string {
			var b strings.Builder
			for i := range 3000 {
				fmt.Fprintf(&b, "definition typ%d {\n relation www: typ0:*\n permission ppp = www\n}\n", i)
			}
			b.WriteString("definition doc {\n relation rrr: typ0")
			for i := 1; i < 3000; i++ {
				fmt.Fprintf(&b, " | typ%d", i)
			}
			b.WriteString("\n relation zzz: typ0\n permission xxx = " + strings.Repeat("rrr->ppp & ", 100) + "zzz\n}\n")
			return b.String()
		}, "doc#xxx", false},
	} {
		t.Run(inv78+": a wide intersection is checked in linear time: "+c.name, func(t *testing.T) {
			text := c.text()
			if len(text) > MaxSchemaBytes {
				t.Fatalf("the schema has %d bytes, more than MaxSchemaBytes", len(text))
			}
			compiled := compileTD03(t, text)
			start := time.Now()
			reach := compiled.wildcardReach()
			elapsed := time.Since(start)
			if reach[c.node] != c.want {
				t.Errorf("%s reaches a wildcard %v, want %v", c.node, reach[c.node], c.want)
			}
			if elapsed > 2*time.Second {
				t.Errorf("the wildcard reach took %v, want well under 2 s", elapsed)
			}
		})
	}
	// The differential test of the security review of PR 294: on random schemas with wildcards, usersets, self
	// loops, cycles, arrows and nested unions, intersections and exclusions, the graph gives the same answer as the
	// fixed point that it replaces, computed pass by pass. STEGO_TEST_REACH_SCHEMAS gives another count, such as the
	// review's 200000.
	t.Run(inv78+": the gate graph gives the answer of the fixed point on random schemas", func(t *testing.T) {
		count := 5000
		if text := os.Getenv("STEGO_TEST_REACH_SCHEMAS"); text != "" {
			n, err := strconv.Atoi(text)
			if err != nil || n < 1 {
				t.Fatalf("STEGO_TEST_REACH_SCHEMAS %q is not a count", text)
			}
			count = n
		}
		r := rand.New(rand.NewPCG(294, 294))
		compiled, sets := 0, 0
		for i := range count {
			text := randomReachSchema(r)
			s, err := CompileAuthzSchema("authz/schema.zed", []byte(text))
			if err != nil {
				continue
			}
			compiled++
			got, want := s.wildcardReach(), s.fixedPointReach()
			for node, v := range want {
				if v && !got[node] {
					t.Fatalf("schema %d: the graph misses %s:\n%s", i, node, text)
				}
			}
			for node, v := range got {
				if v && !want[node] {
					t.Fatalf("schema %d: the graph adds %s:\n%s", i, node, text)
				}
				if v {
					sets++
				}
			}
		}
		if compiled < count/2 || sets == 0 {
			t.Fatalf("only %d of %d random schemas compiled, with %d set nodes", compiled, count, sets)
		}
	})
}

// fixedPointReach is the wildcard reach as it was computed before PR 294, pass by pass over the whole schema until
// no node changes: the oracle of the differential test.
func (s *AuthzSchema) fixedPointReach() map[string]bool {
	reach := map[string]bool{}
	var rewriteReaches func(typeName string, rewrite *core.UsersetRewrite) bool
	child := func(typeName string, c *core.SetOperation_Child) bool {
		switch {
		case c.GetComputedUserset() != nil:
			return reach[typeName+"#"+c.GetComputedUserset().GetRelation()]
		case c.GetTupleToUserset() != nil || c.GetFunctionedTupleToUserset() != nil:
			tupleset, computed := c.GetTupleToUserset().GetTupleset().GetRelation(), c.GetTupleToUserset().GetComputedUserset().GetRelation()
			if f := c.GetFunctionedTupleToUserset(); f != nil {
				tupleset, computed = f.GetTupleset().GetRelation(), f.GetComputedUserset().GetRelation()
			}
			for _, subject := range s.definitions[typeName].subjects[tupleset] {
				if subject.wildcard || reach[subject.namespace+"#"+computed] {
					return true
				}
			}
			return false
		case c.GetUsersetRewrite() != nil:
			return rewriteReaches(typeName, c.GetUsersetRewrite())
		}
		return false
	}
	rewriteReaches = func(typeName string, rewrite *core.UsersetRewrite) bool {
		switch {
		case rewrite.GetUnion() != nil:
			for _, c := range rewrite.GetUnion().GetChild() {
				if child(typeName, c) {
					return true
				}
			}
			return false
		case rewrite.GetIntersection() != nil:
			children := rewrite.GetIntersection().GetChild()
			for _, c := range children {
				if !child(typeName, c) {
					return false
				}
			}
			return len(children) > 0
		case rewrite.GetExclusion() != nil:
			children := rewrite.GetExclusion().GetChild()
			return len(children) > 0 && child(typeName, children[0])
		}
		return false
	}
	relationReaches := func(subjects []schemaSubject) bool {
		for _, subject := range subjects {
			if subject.wildcard || subject.relation != "" && subject.relation != "..." && reach[subject.namespace+"#"+subject.relation] {
				return true
			}
		}
		return false
	}
	for changed := true; changed; {
		changed = false
		for typeName, definition := range s.definitions {
			for name, subjects := range definition.subjects {
				if !reach[typeName+"#"+name] && relationReaches(subjects) {
					reach[typeName+"#"+name], changed = true, true
				}
			}
			for name, rewrite := range definition.rewrites {
				if !reach[typeName+"#"+name] && rewriteReaches(typeName, rewrite) {
					reach[typeName+"#"+name], changed = true, true
				}
			}
		}
	}
	return reach
}

// randomReachSchema gives a random schema of one to five definitions for the differential test, as the security
// review of PR 294 made them.
func randomReachSchema(r *rand.Rand) string {
	defs, rels, perms := 1+r.IntN(5), 1+r.IntN(3), 1+r.IntN(4)
	subject := func(tupleset bool) string {
		d := fmt.Sprintf("def%d", r.IntN(defs))
		switch k := r.IntN(4); {
		case k == 0 && (!tupleset || r.IntN(4) == 0):
			return d + ":*"
		case k == 1 && !tupleset:
			if r.IntN(2) == 0 {
				return fmt.Sprintf("%s#wre%d", d, r.IntN(rels))
			}
			return fmt.Sprintf("%s#per%d", d, r.IntN(perms))
		}
		return d
	}
	leaf := func() string {
		switch r.IntN(4) {
		case 0:
			return fmt.Sprintf("wre%d", r.IntN(rels))
		case 1:
			return fmt.Sprintf("per%d", r.IntN(perms))
		case 2:
			return fmt.Sprintf("tre%d->per%d", r.IntN(rels), r.IntN(perms))
		}
		return fmt.Sprintf("tre%d->wre%d", r.IntN(rels), r.IntN(rels))
	}
	var expr func(depth int) string
	expr = func(depth int) string {
		if depth > 2 || r.IntN(3) == 0 {
			return leaf()
		}
		op := []string{" + ", " & ", " - "}[r.IntN(3)]
		parts := make([]string, 2+r.IntN(2))
		for i := range parts {
			parts[i] = expr(depth + 1)
		}
		return "(" + strings.Join(parts, op) + ")"
	}
	var b strings.Builder
	for d := range defs {
		fmt.Fprintf(&b, "definition def%d {\n", d)
		for i := range rels {
			var w, ts []string
			for range 1 + r.IntN(3) {
				w, ts = append(w, subject(false)), append(ts, subject(true))
			}
			fmt.Fprintf(&b, " relation wre%d: %s\n relation tre%d: %s\n", i, strings.Join(unique(w), " | "), i, strings.Join(unique(ts), " | "))
		}
		for i := range perms {
			fmt.Fprintf(&b, " permission per%d = %s\n", i, expr(0))
		}
		b.WriteString("}\n")
	}
	return b.String()
}
