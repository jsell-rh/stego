package declaration

import (
	"fmt"
	"strings"
	"testing"
	"time"
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
}
