package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/declaration"
)

// The system tests of ST-DECL-UC05, Declare tenancy with its audit settings. Each runs the stego binary built from
// the commit under test in a project of ST-DECL-TD01 with ST-DECL-TD02 changed in one named place, and reads the
// structured output of stego validate --json (ST-DECL-CLI02), with the settings that checking reports.

// tenancyClaim is ST-DECL-TD02 with pooled tenancy, the key org_id and the claim given.
func tenancyClaim(claim string) string {
	return td02 + "tenancy:\n  kind: pooled\n  key: org_id\n  claim: \"" + claim + "\"\n"
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
func acceptedSettings(t *testing.T, service string) *declaration.Settings {
	t.Helper()
	document, run := validateJSON(t, td02Project(t, service))
	if run.code != 0 || !document.Accepted || len(document.Faults) != 0 {
		t.Fatalf("stego validate refused the declaration (exit %d):\n%s%s", run.code, run.stdout, run.stderr)
	}
	if document.Settings == nil {
		t.Fatalf("stego validate reported no settings:\n%s", run.stdout)
	}
	return document.Settings
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

// entitySettings gives the reported settings of the named entity.
func entitySettings(t *testing.T, settings *declaration.Settings, name string) declaration.EntitySettings {
	t.Helper()
	for _, entity := range settings.Entities {
		if entity.Name == name {
			return entity
		}
	}
	t.Fatalf("the settings do not report the entity %s: %+v", name, settings.Entities)
	return declaration.EntitySettings{}
}

// expectDefaults expects each path given to be marked as a default, or not, as marked says.
func expectDefaults(t *testing.T, settings *declaration.Settings, marked bool, paths ...string) {
	t.Helper()
	for _, path := range paths {
		if slices.Contains(settings.Defaults, path) != marked {
			t.Errorf("the setting %s is marked as a default: %v, want %v (defaults %v)", path, !marked, marked, settings.Defaults)
		}
	}
}

// expectOnByDefault expects audit on at level change with a SHA-256 chain and an anchor.
func expectOnByDefault(t *testing.T, audit declaration.AuditSettings) {
	t.Helper()
	if !audit.Enabled || audit.Level != "change" || audit.Chain != "sha-256" || !audit.Anchored {
		t.Errorf("want audit on at level change with a SHA-256 chain and an anchor, got %+v", audit)
	}
}

func TestDeclareTenancyWithAuditSettings(t *testing.T) {
	t.Run("ST-DECL-UC05-FL-T An author declares tenancy, audit, tenant kinds and sensitivity, and checking accepts them with the defaults filled in", func(t *testing.T) {
		service := strings.Replace(tenancyClaim("org.id"), "max_length: 255}", "max_length: 255, sensitivity: pii}", 1)
		service = addEntities(service, "  - name: Plan\n    tenant: shared\n    fields:\n      - {name: label, type: string}\n  - name: Job\n    tenant: system\n    fields:\n      - {name: state, type: string}\n")
		settings := acceptedSettings(t, service)
		if settings.Tenancy.Claim != "org.id" || settings.Tenancy.Kind != "pooled" {
			t.Errorf("want pooled tenancy with the claim path org.id, got %+v", settings.Tenancy)
		}
		expectOnByDefault(t, settings.Audit)
		for name, tenant := range map[string]string{"Todo": "owned", "Plan": "shared", "Job": "system"} {
			if got := entitySettings(t, settings, name).Tenant; got != tenant {
				t.Errorf("%s is reported %s, want %s", name, got, tenant)
			}
		}
		expectDefaults(t, settings, true, "audit.level", "entities.Todo.tenant")
		expectDefaults(t, settings, false, "tenancy.kind", "entities.Plan.tenant", "entities.Job.tenant", "entities.Todo.fields.title.sensitivity")
		if fields := entitySettings(t, settings, "Todo").Fields; len(fields) == 0 || fields[0].Name != "title" || fields[0].Sensitivity != "pii" {
			t.Errorf("want title reported pii, got %+v", fields)
		}
	})

	t.Run("ST-DECL-UC05-R01-T01 A declaration that omits tenancy and audit gets pooled tenancy and audit at level change with a SHA-256 chain and an anchor", func(t *testing.T) {
		settings := acceptedSettings(t, td02)
		if settings.Tenancy.Kind != "pooled" {
			t.Errorf("want tenancy kind pooled, got %+v", settings.Tenancy)
		}
		expectOnByDefault(t, settings.Audit)
		expectDefaults(t, settings, true, "tenancy.kind", "audit.enabled", "audit.level")
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
		settings := acceptedSettings(t, td02+"tenancy: {kind: single, reason: Internal tool for one organisation}\naudit: {enabled: false, reason: Stateless proxy with no data of its own}\n")
		if len(settings.DefaultsLeft) != 2 {
			t.Errorf("want the two defaults left reported with their reasons, got %+v", settings.DefaultsLeft)
		}
	})

	t.Run("ST-DECL-UC05-R03-T01 A tenancy claim that is a path in the verified token is accepted", func(t *testing.T) {
		if settings := acceptedSettings(t, tenancyClaim("org.id")); settings.Tenancy.Claim != "org.id" {
			t.Errorf("want the claim path org.id, got %+v", settings.Tenancy)
		}
	})

	for id, claim := range map[string]string{"ST-DECL-UC05-R03-T02 A tenancy claim taken from a request header": "header.X-Tenant-ID", "ST-DECL-UC05-R03-T03 A tenancy claim taken from a body field": "body.org_id"} {
		t.Run(id+" is refused, naming the claim", func(t *testing.T) {
			fault := refusedFault(t, tenancyClaim(claim), "tenancy.claim")
			if fault.Found != claim || !strings.Contains(fault.Message, "request part") {
				t.Errorf("want a fault that names the claim %s and says that it names a request part, got %+v", claim, fault)
			}
		})
	}

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
		if got := entitySettings(t, acceptedSettings(t, td02), "Todo").Tenant; got != "owned" {
			t.Errorf("Todo is reported %s, want owned", got)
		}
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
