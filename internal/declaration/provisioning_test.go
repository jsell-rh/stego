package declaration

import "testing"

func TestTenantProvisioning(t *testing.T) {
	const inv78 = "ST-DECL-INV78-T A pooled service without its tenant provisioning, or with a public or unknown permission, is refused"
	const line = "  provisioning: {resource_type: \"document\", provision: edit, retire: edit}\n"
	service, schema := td03Files(t)
	t.Run(inv78+": TD03 with the tenant provisioning on document with edit and edit is accepted", func(t *testing.T) {
		expectAccepted(t, checkTD03(t, service, schema))
	})
	for _, c := range []struct {
		name, provisioning, path string
		texts                []string
	}{
		{"no tenant provisioning", "", "tenancy.provisioning", []string{"tenant provisioning", "add tenancy.provisioning"}},
		{"no Retire permission", "  provisioning: {resource_type: \"document\", provision: edit}\n", "tenancy.provisioning.retire", []string{"tenant provisioning", "Retire permission"}},
		{"the Provision permission approve, which the schema lacks", "  provisioning: {resource_type: \"document\", provision: approve, retire: edit}\n", "tenancy.provisioning.provision", []string{"tenant provisioning", "approve", "lacks"}},
		{"the Provision permission declared public", "  provisioning: {resource_type: \"document\", provision: public, retire: edit}\n", "tenancy.provisioning.provision", []string{"tenant provisioning", "public"}},
		{"a resource type that the schema lacks", "  provisioning: {resource_type: tenant_admin, provision: edit, retire: edit}\n", "tenancy.provisioning.resource_type", []string{"tenant provisioning", "tenant_admin", "lacks"}},
	} {
		t.Run(inv78+": "+c.name, func(t *testing.T) {
			fault := expectOneFault(t, checkTD03(t, change(t, service, line, c.provisioning), schema), "tenant-provisioning", c.texts...)
			if fault.Path != c.path {
				t.Errorf("want the fault at %s, got %+v", c.path, fault)
			}
		})
	}
	t.Run(inv78+": a pooled service with no authorization needs only the provisioning, and a single tenancy declares none", func(t *testing.T) {
		unbound := withoutAuthorizationParts(t, change(t, service, line, "  provisioning: {}\n"))
		for _, fault := range checkTD03(t, unbound, schema) {
			if fault.Rule == "tenant-provisioning" {
				t.Errorf("a pooled service with no authorization and an empty provisioning gets %+v", fault)
			}
		}
		single := change(t, service, "tenancy:\n  kind: pooled\n  key: tenant_id\n  claim: tenant\n", "tenancy:\n  kind: single\n  reason: one customer per deployment\n")
		expectOneFault(t, Validate(readTD03(t, single)).All(), "tenant-provisioning", "single", "remove tenancy.provisioning")
	})
}
