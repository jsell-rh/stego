package declaration

import (
	"strconv"
	"strings"
	"testing"
)

// pooledWithClaim is the tenancy setting of a pooled service with the key org_id and the claim given.
func pooledWithClaim(claim string) string {
	return "tenancy:\n  kind: pooled\n  key: org_id\n  claim: " + strconv.Quote(claim) + "\n  provisioning: {}\n"
}

func TestTenancyClaim(t *testing.T) {
	for _, claim := range []string{"org.id", "tenant", "https://example.com/claims:org_id"} {
		t.Run("ST-DECL-INV19-T the token claim path "+claim+" is accepted", func(t *testing.T) {
			if faults := faultsOf(readTD02(t, pooledWithClaim(claim))); len(faults) != 0 {
				t.Fatalf("want no fault, got %+v", faults)
			}
		})
	}
	requestParts := map[string]string{
		"header.X-Tenant-ID": "header", "Headers.x-tenant-id": "headers", "body.org_id": "body",
		"request.body.org_id": "request", "query.tenant": "query", "cookie.tenant": "cookie", "path.org": "path",
	}
	for claim, part := range requestParts {
		t.Run("ST-DECL-INV19-T the claim "+claim+" names a request part and is refused", func(t *testing.T) {
			faults := faultsOf(readTD02(t, pooledWithClaim(claim)))
			if len(faults) != 1 || faults[0].Path != "tenancy.claim" || faults[0].Found != claim || !strings.Contains(faults[0].Message, "request part "+part) {
				t.Fatalf("want one fault that names the claim %s and the request part %s, got %+v", claim, part, faults)
			}
		})
	}
	for _, claim := range []string{"org..id", ".org", "org id", ""} {
		t.Run("ST-DECL-INV19-T the claim "+strconv.Quote(claim)+" is not a token claim path and is refused", func(t *testing.T) {
			faults := faultsOf(readTD02(t, pooledWithClaim(claim)))
			if len(faults) != 1 || faults[0].Path != "tenancy.claim" || faults[0].Found != claim || !strings.Contains(faults[0].Message, "is not a token claim path") {
				t.Fatalf("want one fault that names the claim as not a token claim path, got %+v", faults)
			}
		})
	}
}

func TestTenantFromClientField(t *testing.T) {
	cases := map[string]bool{"header.X-Tenant-ID": true, "BODY.org_id": true, "request.body.org_id": true, "org.id": false, "headerx.org": false}
	for claim, satisfies := range cases {
		t.Run("ST-DECL-SP02-T the claim "+claim, func(t *testing.T) {
			if got := TenantFromClientField(readTD02(t, pooledWithClaim(claim))); got != satisfies {
				t.Fatalf("TenantFromClientField = %v, want %v", got, satisfies)
			}
		})
	}
}

// provisioner is the controller of ST-DECL-INV20-T, complete in each field but its tenants.
func provisioner(tenants string) string {
	return "controllers:\n  - {name: provisioner, sources: [scan, watch], resync: 10m, replicas: 2, lease_ttl: 15s, tenants: " + tenants + "}\n"
}

// withEntity adds an entity with the tenant given, and a collection over it with the operations given, to TD02.
func withEntity(entity, tenant, collection, operations string) string {
	service := strings.Replace(td02, "collections:\n", "  - name: "+entity+"\n    tenant: "+tenant+"\n    fields:\n      - {name: label, type: string}\ncollections:\n", 1)
	return service + "  " + collection + ":\n    entity: " + entity + "\n    operations: [" + operations + "]\n"
}

// plansBinding is a binding on plans at before_create that runs as the principal given.
func plansBinding(runsAs string) string {
	return "slots:\n  - collection: plans\n    slot: before_create\n    gate: [policy]\n    runs_as: " + runsAs + "\n"
}

func TestTenantlessPaths(t *testing.T) {
	t.Run("ST-DECL-INV20-T a controller with tenants none is refused, naming the controller", func(t *testing.T) {
		faults := faultsOf(readTD02(t, provisioner("none")))
		if len(faults) != 1 || faults[0].Found != "none" || !strings.Contains(faults[0].Message, `"provisioner"`) {
			t.Fatalf("want one fault that names the controller provisioner, got %+v", faults)
		}
	})
	t.Run("ST-DECL-INV20-T a collection over a system entity is refused, naming the collection and the entity", func(t *testing.T) {
		faults := faultsOf(readDeclaration(t, withEntity("Region", "system", "regions", "read, list")))
		if len(faults) != 1 || !strings.Contains(faults[0].Message, `"regions"`) || !strings.Contains(faults[0].Message, `"Region"`) {
			t.Fatalf("want one fault that names regions and Region, got %+v", faults)
		}
	})
	t.Run("ST-DECL-INV20-T a tenant binding that writes a shared entity is refused, naming the binding", func(t *testing.T) {
		faults := faultsOf(readDeclaration(t, withEntity("Plan", "shared", "plans", "create")+plansBinding("tenant")))
		if len(faults) != 1 || faults[0].Path != "slots[0].runs_as" || !strings.Contains(faults[0].Message, `"plans"`) {
			t.Fatalf("want one fault that names the binding on plans, got %+v", faults)
		}
	})
	t.Run("ST-DECL-INV20-T a controller with tenants each is accepted", func(t *testing.T) {
		if faults := faultsOf(readTD02(t, provisioner("each"))); len(faults) != 0 {
			t.Fatalf("want no fault, got %+v", faults)
		}
	})
	t.Run("ST-DECL-INV20-T the binding on plans that runs as system is accepted", func(t *testing.T) {
		if faults := faultsOf(readDeclaration(t, withEntity("Plan", "shared", "plans", "create")+plansBinding("system"))); len(faults) != 0 {
			t.Fatalf("want no fault, got %+v", faults)
		}
	})
}

func TestTenantlessPath(t *testing.T) {
	cases := []struct {
		name, service string
		satisfies     bool
	}{
		{"a controller with tenants none", td02 + provisioner("none"), true},
		{"a collection over a system entity", withEntity("Region", "system", "regions", "read"), true},
		{"a tenant binding on a collection with create over a shared entity", withEntity("Plan", "shared", "plans", "create") + plansBinding("tenant"), true},
		{"a controller with tenants each", td02 + provisioner("each"), false},
		{"a collection over an owned entity", withEntity("Region", "owned", "regions", "read"), false},
		{"the binding run as system", withEntity("Plan", "shared", "plans", "create") + plansBinding("system"), false},
	}
	for _, c := range cases {
		t.Run("ST-DECL-SP03-T "+c.name, func(t *testing.T) {
			if got := TenantlessPath(readDeclaration(t, c.service)); got != c.satisfies {
				t.Fatalf("TenantlessPath = %v, want %v", got, c.satisfies)
			}
		})
	}
}
