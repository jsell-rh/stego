package declaration

import (
	"testing"
	"testing/fstest"
)

// referencesSchema is the schema of the reference test: an order refers to its customer and its product.
const referencesSchema = `definition user {}
definition customer {
	relation owner: user
	permission place_order = owner
	permission view = owner
}
definition product {
	relation viewer: user
	permission view = viewer
}
definition order {
	relation owner: user
	relation customer: customer
	relation product: product
	permission view = owner
}
`

// referencesService is a pooled declaration with authorization whose Order refers to Customer and Product.
const referencesService = `kind: service
name: shop
archetype: rest-crud
language: go
base_path: /api/shop/v1
tenancy:
  kind: pooled
  key: tenant_id
  claim: tenant
authorization:
  provider: spicedb
  schema_path: authz/schema.zed
  subject_from_identity: user
entities:
  - name: Customer
    resource_type: customer
    permissions: {read: view, list: view}
    fields:
      - name: name
        type: string
  - name: Product
    resource_type: product
    permissions: {read: view, list: view}
    fields:
      - name: title
        type: string
  - name: Order
    resource_type: order
    permissions: {read: view, list: view}
    references:
      customer_id: {relation: customer, permission: place_order}
      product_id: {relation: product, permission: view}
    fields:
      - name: customer_id
        type: ref
        to: Customer
      - name: product_id
        type: ref
        to: Product
`

func TestReferences(t *testing.T) {
	t.Run("ST-DECL-INV85-T A reference to a protected entity with no relation, no permission or an undeclared name is refused, naming the field", func(t *testing.T) {
		check := func(service string) []ValidationError {
			t.Helper()
			decl := readTD03(t, service)
			return validateReferences(decl.Entities, compileTD03(t, referencesSchema))
		}
		expectAccepted(t, check(referencesService))
		// Through the whole authorization check, the accepted declaration gets no reference fault either.
		decl := readTD03(t, referencesService)
		for _, fault := range CheckAuthorization(decl, fstest.MapFS{"authz/schema.zed": {Data: []byte(referencesSchema)}}) {
			if fault.Rule == "entity-references" {
				t.Fatalf("the accepted declaration got %+v", fault)
			}
		}
		expectOneFault(t, check(change(t, referencesService, "      product_id: {relation: product, permission: view}\n", "")),
			"entity-references", "product_id")
		expectOneFault(t, check(change(t, referencesService, "{relation: product, permission: view}", "{permission: view}")),
			"entity-references", "product_id", "relation")
		expectOneFault(t, check(change(t, referencesService, "{relation: product, permission: view}", "{relation: product}")),
			"entity-references", "product_id", "permission")
		expectOneFault(t, check(change(t, referencesService, "{relation: product, permission: view}", "{relation: maker, permission: view}")),
			"entity-references", "product_id", "maker")
		expectOneFault(t, check(change(t, referencesService, "{relation: customer, permission: place_order}", "{relation: customer, permission: refund}")),
			"entity-references", "customer_id", "refund")
		// A relation of the schema for another subject type is not the reference's relation.
		expectOneFault(t, check(change(t, referencesService, "{relation: product, permission: view}", "{relation: customer, permission: view}")),
			"entity-references", "product_id", "customer")
		// References of a field that is no reference to a protected entity are refused too.
		expectOneFault(t, check(change(t, referencesService, "      product_id: {relation: product, permission: view}\n",
			"      product_id: {relation: product, permission: view}\n      note: {relation: product, permission: view}\n")),
			"entity-references", "note")
	})
}
