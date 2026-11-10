package main

import (
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/declaration"
)

// The system tests of ST-DECL-UC05, Declare tenancy with its audit settings. Each runs the stego binary built from
// the commit under test in a project of ST-DECL-TD01 with ST-DECL-TD02 changed in one named place, and reads the
// structured output of stego validate --json (ST-DECL-CLI02), with the settings that checking reports.

// tenancyClaim is ST-DECL-TD02 with pooled tenancy, the key org_id and the claim given. The stated tenancy selects
// the tenancy component, which is alpha, so the declaration accepts it (ST-GEN-INV37).
func tenancyClaim(claim string) string {
	return td02 + "tenancy:\n  kind: pooled\n  key: org_id\n  claim: \"" + claim + "\"\n" +
		acceptanceOf("tenancy", "tenancy builder pilot")
}

// withProvisioner is ST-DECL-TD02 with the controller provisioner, complete in each field but its tenants.
func withProvisioner(tenants string) string {
	return td02 + "controllers:\n  - {name: provisioner, sources: [scan], resync: 10m, replicas: 2, lease_ttl: 15s, tenants: " + tenants + "}\n"
}

// addEntities adds the entities given to ST-DECL-TD02, after Todo.
func addEntities(service, entities string) string {
	return strings.Replace(service, "collections:\n", entities+"collections:\n", 1)
}

// acceptedSettings runs stego validate --json, expects exit 0 and no fault, and gives the settings it reports.
func acceptedSettings(t *testing.T, service string) declaration.FaultsDocument {
	t.Helper()
	document, run := validateJSON(t, td02Project(t, service))
	if run.code != 0 || !document.Accepted || len(document.Faults) != 0 {
		t.Fatalf("stego validate refused the declaration (exit %d):\n%s%s", run.code, run.stdout, run.stderr)
	}
	return document
}

// refusedFault runs stego validate --json, expects a non-zero exit, and gives the one fault at the path given.
func refusedFault(t *testing.T, service, path string) declaration.ValidationError {
	t.Helper()
	document, run := validateJSON(t, td02Project(t, service))
	if run.code == 0 || document.Accepted {
		t.Fatalf("stego validate accepted the declaration:\n%s", run.stdout)
	}
	var found []declaration.ValidationError
	for _, fault := range document.Faults {
		if fault.Path == path {
			found = append(found, fault)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want one fault at %s, got:\n%s", path, run.stdout)
	}
	return found[0]
}

// expectSetting expects the reported setting at the path to have the value given, and to be marked as a default
// or not, as byDefault says.
func expectSetting(t *testing.T, document declaration.FaultsDocument, path string, value any, byDefault bool) {
	t.Helper()
	setting := reportedSetting(t, document, path)
	if setting.Value != value || setting.Default != byDefault {
		t.Errorf("want %s %v with default %v, got %+v", path, value, byDefault, setting)
	}
}

// expectOnByDefault expects audit on at level change with a SHA-256 chain and an anchor, each a default.
func expectOnByDefault(t *testing.T, document declaration.FaultsDocument) {
	t.Helper()
	expectSetting(t, document, "audit.enabled", true, true)
	expectSetting(t, document, "audit.level", "change", true)
	expectSetting(t, document, "audit.chain", "sha-256", true)
	expectSetting(t, document, "audit.anchor", true, true)
}

func TestDeclareTenancyWithAuditSettings(t *testing.T) {
	t.Run("ST-DECL-UC05-FL-T An author declares tenancy, audit, tenant kinds and sensitivity, and checking accepts them with the defaults filled in", func(t *testing.T) {
		service := strings.Replace(tenancyClaim("org.id"), "max_length: 255}", "max_length: 255, sensitivity: pii}", 1)
		service = addEntities(service, "  - name: Plan\n    tenant: shared\n    fields:\n      - {name: label, type: string}\n  - name: Job\n    tenant: system\n    fields:\n      - {name: state, type: string}\n")
		document := acceptedSettings(t, service)
		expectSetting(t, document, "tenancy.kind", "pooled", false)
		expectSetting(t, document, "tenancy.claim", "org.id", false)
		expectOnByDefault(t, document)
		expectSetting(t, document, "entities[0].tenant", "owned", true)
		expectSetting(t, document, "entities[1].tenant", "shared", false)
		expectSetting(t, document, "entities[2].tenant", "system", false)
		expectSetting(t, document, "entities[0].fields[0].sensitivity", "pii", false)
		expectSetting(t, document, "entities[0].fields[1].sensitivity", "public", true)
	})

	t.Run("ST-DECL-UC05-R01-T01 A declaration that omits tenancy and audit gets pooled tenancy and audit at level change with a SHA-256 chain and an anchor", func(t *testing.T) {
		document := acceptedSettings(t, td02)
		expectSetting(t, document, "tenancy.kind", "pooled", true)
		expectSetting(t, document, "tenancy.key", "tenant_id", true)
		expectSetting(t, document, "tenancy.claim", "tenant_id", true)
		expectOnByDefault(t, document)
	})

	t.Run("ST-DECL-UC05-R02-T01 Tenancy kind single with no reason is refused, naming the setting", func(t *testing.T) {
		fault := refusedFault(t, td02+"tenancy: {kind: single}\n", "tenancy.reason")
		if !strings.Contains(fault.Message, "tenancy") || !strings.Contains(fault.Message, "reason") {
			t.Errorf("the fault does not say that the tenancy setting needs a reason: %+v", fault)
		}
	})

	t.Run("ST-DECL-UC05-R02-T02 Audit off with no reason is refused, naming the setting", func(t *testing.T) {
		fault := refusedFault(t, td02+"audit: {enabled: false}\n", "audit.reason")
		if !strings.Contains(fault.Message, "audit") || !strings.Contains(fault.Message, "reason") {
			t.Errorf("the fault does not say that the audit setting needs a reason: %+v", fault)
		}
	})

	t.Run("ST-DECL-UC05-R02-T03 A default left with a reason is accepted", func(t *testing.T) {
		document := acceptedSettings(t, td02+"tenancy: {kind: single, reason: Internal tool for one organisation}\naudit: {enabled: false, reason: Stateless proxy with no data of its own}\n")
		expectSetting(t, document, "tenancy.kind", "single", false)
		expectSetting(t, document, "tenancy.reason", "Internal tool for one organisation", false)
		expectSetting(t, document, "audit.enabled", false, false)
		expectSetting(t, document, "audit.reason", "Stateless proxy with no data of its own", false)
	})

	t.Run("ST-DECL-UC05-R03-T01 A tenancy claim that is a path in the verified token is accepted", func(t *testing.T) {
		expectSetting(t, acceptedSettings(t, tenancyClaim("org.id")), "tenancy.claim", "org.id", false)
	})

	requestPart := func(t *testing.T, claim string) {
		t.Helper()
		fault := refusedFault(t, tenancyClaim(claim), "tenancy.claim")
		if fault.Found != claim || !strings.Contains(fault.Message, "request part") {
			t.Errorf("want a fault that names the claim %s and says that it names a request part, got %+v", claim, fault)
		}
	}
	t.Run("ST-DECL-UC05-R03-T02 A tenancy claim taken from a request header is refused, naming the claim", func(t *testing.T) {
		requestPart(t, "header.X-Tenant-ID")
	})
	t.Run("ST-DECL-UC05-R03-T03 A tenancy claim taken from a body field is refused, naming the claim", func(t *testing.T) {
		requestPart(t, "body.org_id")
	})

	t.Run("ST-DECL-UC05-R03-T04 A tenancy claim that does not follow the token claim path grammar is refused, naming the claim", func(t *testing.T) {
		for _, claim := range []string{"org..id", "org id"} {
			fault := refusedFault(t, tenancyClaim(claim), "tenancy.claim")
			if fault.Found != claim || !strings.Contains(fault.Message, "not a token claim path") {
				t.Errorf("want a fault that names the claim %q as not a token claim path, got %+v", claim, fault)
			}
		}
	})

	t.Run("ST-DECL-UC05-R04-T01 An entity tenant other than owned, shared or system is refused, and an omitted one is owned", func(t *testing.T) {
		fault := refusedFault(t, strings.Replace(td02, "  - name: Todo\n", "  - name: Todo\n    tenant: global\n", 1), "entities[0].tenant")
		if fault.Found != "global" || !strings.Contains(fault.Message, `"Todo"`) || strings.Join(fault.Allowed, ",") != "owned,shared,system" {
			t.Errorf("want a fault that names Todo, global and owned, shared and system, got %+v", fault)
		}
		expectSetting(t, acceptedSettings(t, td02), "entities[0].tenant", "owned", true)
	})

	t.Run("ST-DECL-UC05-R04-T02 A collection over a system entity is refused, naming the collection and the entity", func(t *testing.T) {
		service := addEntities(td02, "  - name: Region\n    tenant: system\n    fields:\n      - {name: code, type: string}\n") + "  regions:\n    entity: Region\n    operations: [read, list]\n"
		fault := refusedFault(t, service, "collections.regions.entity")
		if !strings.Contains(fault.Message, `"regions"`) || !strings.Contains(fault.Message, `"Region"`) {
			t.Errorf("want a fault that names regions and Region, got %+v", fault)
		}
	})

	t.Run("ST-DECL-UC05-R05-T01 A controller that declares tenants none is refused, naming the controller", func(t *testing.T) {
		fault := refusedFault(t, withProvisioner("none"), "controllers[0].tenants")
		if fault.Found != "none" || !strings.Contains(fault.Message, `"provisioner"`) {
			t.Errorf("want a fault that names the controller provisioner and the value none, got %+v", fault)
		}
	})

	t.Run("ST-DECL-UC05-R05-T02 A controller that declares tenants each is accepted", func(t *testing.T) {
		acceptedSettings(t, withProvisioner("each"))
	})
}
