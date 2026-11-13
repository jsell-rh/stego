package main

import (
	"slices"
	"strings"
	"testing"
)

// td04Plain is ST-DECL-TD02 with the ledger of ST-DECL-TD04: the entity Wallet of kind ledger in EUR with the
// transfer codes topup, purchase and refund, two phase, the pending timeout 15m and a snapshot every 3 entries; the
// accounts customer_wallet and settlement; and the collection transfers over Wallet with the six ledger operations.
// Wallet states its own permissions, because the runtime settings give each other entity the data operations only.
var td04Plain = strings.Replace(td02Plain, "collections:\n", `  - name: Wallet
    kind: ledger
    resource_type: wallet
    permissions: {create: public, post: edit, void: edit, reverse: edit, read: view, list: view}
    public_reasons: {create: a new transfer has no resource to decide on before it exists}
    fields: []
    ledger:
      currency: EUR
      transfer_codes: [topup, purchase, refund]
      two_phase: true
      pending_timeout: 15m
      snapshot_every: 3
      accounts:
        - {name: customer_wallet, normal: credit, min_balance: {amount: 0, currency: EUR}, owner: customer, hot: false}
        - {name: settlement, normal: debit, hot: true}
collections:
`, 1) + `  transfers:
    entity: Wallet
    operations: [create, post, void, reverse, read, list]
`

// td04Changed gives ST-DECL-TD04 on the runtime data path with each old text replaced by its new text, once each.
func td04Changed(t *testing.T, replacements ...string) string {
	t.Helper()
	service := td04Plain
	for i := 0; i+1 < len(replacements); i += 2 {
		if !strings.Contains(service, replacements[i]) {
			t.Fatalf("ST-DECL-TD04 holds no %q", replacements[i])
		}
		service = strings.Replace(service, replacements[i], replacements[i+1], 1)
	}
	return onRuntimePath(service)
}

// todoPrice is the change of ST-DECL-TD04 that adds the field price to Todo, written as given.
func todoPrice(price string) []string {
	return []string{"      - {name: done, type: bool, default: false}\n", "      - {name: done, type: bool, default: false}\n      - " + price + "\n"}
}

// refusedNaming runs stego validate on the declaration and expects a refusal with one fault at the path whose
// message names each of the names.
func refusedNaming(t *testing.T, service, path string, names ...string) {
	t.Helper()
	fault := refusedFault(t, service, path)
	for _, name := range names {
		if !strings.Contains(fault.Message, name) {
			t.Errorf("the fault at %s does not name %s: %+v", path, name, fault)
		}
	}
}

func TestDeclareALedger(t *testing.T) {
	t.Parallel()
	const ledger = "entities[1].ledger"
	const customerWallet = ledger + ".accounts[0]"
	const settlement = ledger + ".accounts[1]"
	const price = "entities[0].fields[2]"

	t.Run("ST-DECL-UC08-R01-T02 A ledger with no currency is refused", func(t *testing.T) {
		refusedNaming(t, td04Changed(t, "      currency: EUR\n", ""), ledger+".currency", `"Wallet"`, "no currency")
	})
	t.Run("ST-DECL-UC08-R01-T03 A ledger with two currencies is refused", func(t *testing.T) {
		refusedNaming(t, td04Changed(t, "      currency: EUR\n", "      currency: [EUR, USD]\n"), ledger+".currency", `"Wallet"`, "EUR and USD")
	})
	t.Run("ST-DECL-UC08-R01-T04 A ledger with a currency that is not ISO 4217 is refused", func(t *testing.T) {
		refusedNaming(t, td04Changed(t, "      currency: EUR\n", "      currency: EURO\n"), ledger+".currency", `"Wallet"`, `"EURO"`)
	})
	t.Run("ST-DECL-UC08-R01-T05 A ledger with no transfer codes is refused", func(t *testing.T) {
		refusedNaming(t, td04Changed(t, "      transfer_codes: [topup, purchase, refund]\n", ""), ledger+".transfer_codes", `"Wallet"`, "no transfer codes")
	})
	t.Run("ST-DECL-UC08-R01-T06 A ledger with no two-phase rule is refused", func(t *testing.T) {
		refusedNaming(t, td04Changed(t, "      two_phase: true\n", ""), ledger+".two_phase", `"Wallet"`, "does not state two phase")
	})

	t.Run("ST-DECL-UC08-R02-T01 An account with no normal balance is refused, naming the account", func(t *testing.T) {
		refusedNaming(t, td04Changed(t, "customer_wallet, normal: credit,", "customer_wallet,"), customerWallet+".normal", `"customer_wallet"`, "normal")
	})
	t.Run("ST-DECL-UC08-R02-T02 A normal balance other than debit or credit is refused, with the allowed values", func(t *testing.T) {
		service := td04Changed(t, "settlement, normal: debit", "settlement, normal: both")
		refusedNaming(t, service, settlement+".normal", `"settlement"`, `"both"`)
		if fault := refusedFault(t, service, settlement+".normal"); fault.Found != "both" || !slices.Equal(fault.Allowed, []string{"debit", "credit"}) {
			t.Errorf("want the value both found and debit and credit allowed, got %+v", fault)
		}
	})
	t.Run("ST-DECL-UC08-R02-T04 A minimum balance that is not Money is refused", func(t *testing.T) {
		refusedNaming(t, td04Changed(t, "{amount: 0, currency: EUR}", "{amount: 10.5, currency: EUR}"), customerWallet+".min_balance.amount", `"customer_wallet"`, "minimum balance", `"10.5"`)
		refusedNaming(t, td04Changed(t, "{amount: 0, currency: EUR}", "{amount: 0}"), customerWallet+".min_balance.currency", `"customer_wallet"`, "minimum balance")
	})

	t.Run("ST-DECL-UC08-R03-T01 A money field typed as a float is refused, naming the field", func(t *testing.T) {
		refusedNaming(t, td04Changed(t, todoPrice("{name: price, type: float, currency: EUR}")...), price+".type", `"Todo"`, `"price"`)
	})
	t.Run("ST-DECL-UC08-R03-T03 A money field with no currency is refused, naming the field", func(t *testing.T) {
		refusedNaming(t, td04Changed(t, todoPrice("{name: price, type: money}")...), price+".currency", `"Todo"`, `"price"`)
	})

	t.Run("ST-DECL-UC08-R04-T01 A ledger collection that allows update is refused, naming the collection and the operation", func(t *testing.T) {
		refusedNaming(t, td04Changed(t, "reverse, read, list]", "reverse, read, list, update]"), "collections.transfers.operations[6]", `"transfers"`, `"update"`)
	})
	t.Run("ST-DECL-UC08-R04-T02 A ledger collection that allows delete is refused, naming the collection and the operation", func(t *testing.T) {
		refusedNaming(t, td04Changed(t, "reverse, read, list]", "reverse, read, list, delete]"), "collections.transfers.operations[6]", `"transfers"`, `"delete"`)
	})

	t.Run("ST-DECL-UC08-R05-T02 A ledger account with a balance field is refused as a field the format does not hold, naming the field", func(t *testing.T) {
		service := td04Changed(t, "customer_wallet, normal: credit,", "customer_wallet, normal: credit, balance: 0,")
		refusedNaming(t, service, customerWallet+".balance", `"balance"`, `"customer_wallet"`, "the declaration format does not hold")
		if fault := refusedFault(t, service, customerWallet+".balance"); fault.Rule != "undeclared-field" {
			t.Errorf("want the fault of the strict input stage, got %+v", fault)
		}
	})
}
