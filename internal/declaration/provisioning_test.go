package declaration

import (
	"strings"
	"testing"
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
}
