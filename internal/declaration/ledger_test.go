package declaration

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/parser"
	"github.com/jsell-rh/stego/internal/types"
)

// td04 is the ledger declaration of ST-DECL-TD04: ST-DECL-TD02 and the entity Wallet of kind ledger in EUR, with
// the accounts customer_wallet and settlement, and the collection transfers over Wallet.
const td04 = `kind: service
name: todo-svc
archetype: rest-crud
language: go
base_path: /api/todo/v1
entities:
  - name: Todo
    fields:
      - {name: title, type: string, min_length: 1, max_length: 255}
      - {name: done, type: bool, default: false}
  - name: Wallet
    kind: ledger
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
  todos:
    entity: Todo
    operations: [create, read, update, delete, list]
  transfers:
    entity: Wallet
    operations: [create, post, void, reverse, read, list]
`

// td04Changed reads ST-DECL-TD04 with each old text replaced by its new text, once each.
func td04Changed(t *testing.T, replacements ...string) *types.ServiceDeclaration {
	t.Helper()
	data := td04
	for i := 0; i+1 < len(replacements); i += 2 {
		if !strings.Contains(data, replacements[i]) {
			t.Fatalf("ST-DECL-TD04 holds no %q", replacements[i])
		}
		data = strings.Replace(data, replacements[i], replacements[i+1], 1)
	}
	service, undeclared, err := parser.ReadServiceDeclaration([]byte(data), "service.yaml")
	if err != nil || len(undeclared) != 0 {
		t.Fatalf("read the declaration: %v %v", err, undeclared)
	}
	return service
}

// withPrice is the change of ST-DECL-TD04 that adds the field price to Todo, written as given.
func withPrice(price string) []string {
	return []string{"      - {name: done, type: bool, default: false}\n", "      - {name: done, type: bool, default: false}\n      - " + price + "\n"}
}

// faultAt gives the faults whose path is the path given.
func faultAt(service *types.ServiceDeclaration, path string) []ValidationError {
	var faults []ValidationError
	for _, fault := range Validate(service).All() {
		if fault.Path == path {
			faults = append(faults, fault)
		}
	}
	return faults
}

// inv26 is the ID and title of ST-DECL-INV26-T, which each of its cases names.
const inv26 = "ST-DECL-INV26-T A float money field, a currency that is not ISO 4217, two currencies in a ledger and an account with no normal balance of debit or credit are refused"

func TestMoneyAndLedgerCurrency(t *testing.T) {
	{
		const price = "entities[0].fields[2]"
		const account = "entities[1].ledger.accounts"
		for _, refused := range []struct {
			name, path string
			change     []string
			names      []string
		}{
			{"a money field price typed as a float", price + ".type", withPrice("{name: price, type: float, currency: EUR}"), []string{`"Todo"`, `"price"`, "integer minor units"}},
			{"a money field price with the currency EURO", price + ".currency", withPrice("{name: price, type: money, currency: EURO}"), []string{`"Todo"`, `"price"`, `"EURO"`}},
			{"the ledger currency EURO", "entities[1].ledger.currency", []string{"currency: EUR\n", "currency: EURO\n"}, []string{`"Wallet"`, `"EURO"`}},
			{"the ledger currencies EUR and USD", "entities[1].ledger.currency", []string{"currency: EUR\n", "currency: [EUR, USD]\n"}, []string{`"Wallet"`, "EUR and USD"}},
			{"the account customer_wallet with no normal balance", account + "[0].normal", []string{"customer_wallet, normal: credit,", "customer_wallet,"}, []string{`"customer_wallet"`, "debit or credit"}},
			{"the account settlement with the normal balance both", account + "[1].normal", []string{"normal: debit", "normal: both"}, []string{`"settlement"`, `"both"`, "debit or credit"}},
		} {
			t.Run(inv26+": "+refused.name, func(t *testing.T) {
				faults := faultAt(td04Changed(t, refused.change...), refused.path)
				if len(faults) != 1 {
					t.Fatalf("want one fault at %s, got %+v", refused.path, Validate(td04Changed(t, refused.change...)).All())
				}
				for _, name := range refused.names {
					if !strings.Contains(faults[0].Message, name) {
						t.Errorf("the fault does not name %s: %+v", name, faults[0])
					}
				}
			})
		}
	}
	t.Run(inv26+": the hot account settlement with a minimum balance", func(t *testing.T) {
		faults := faultAt(td04Changed(t, "settlement, normal: debit, hot: true", "settlement, normal: debit, hot: true, min_balance: {amount: 0, currency: EUR}"), "entities[1].ledger.accounts[1].min_balance")
		if len(faults) != 1 || !strings.Contains(faults[0].Message, `"settlement"`) || !strings.Contains(faults[0].Message, "hot") {
			t.Errorf("want one fault that names settlement and hot, got %+v", faults)
		}
	})
	t.Run(inv26+": an amount beyond 2^53 minor units is refused, and one at the bound is accepted", func(t *testing.T) {
		const bound, beyond = "9007199254740992", "9007199254740993"
		for amount, refused := range map[string]bool{bound: false, "-" + bound: false, beyond: true, "-" + beyond: true} {
			minimum := faultAt(td04Changed(t, "{amount: 0, currency: EUR}", "{amount: "+amount+", currency: EUR}"), "entities[1].ledger.accounts[0].min_balance.amount")
			fallback := faultAt(td04Changed(t, withPrice("{name: price, type: money, currency: EUR, default: "+amount+"}")...), "entities[0].fields[2].default")
			if (len(minimum) == 1) != refused || (len(fallback) == 1) != refused {
				t.Errorf("amount %s: want refused %v, got the minimum balance faults %+v and the default faults %+v", amount, refused, minimum, fallback)
			}
		}
	})
	t.Run(inv26+": a money field price as integer minor units in EUR is accepted", func(t *testing.T) {
		if faults := Validate(td04Changed(t, withPrice("{name: price, type: money, currency: EUR}")...)).All(); len(faults) != 0 {
			t.Errorf("want no fault, got %+v", faults)
		}
	})
	t.Run(inv26+": ST-DECL-TD04 unchanged is accepted", func(t *testing.T) {
		if faults := Validate(td04Changed(t)).All(); len(faults) != 0 {
			t.Errorf("want no fault, got %+v", faults)
		}
	})

	t.Run(inv26+": the currencies that checking accepts are the currencies of the runtime Money type", func(t *testing.T) {
		// Checking imports no runtime package, so it holds its own copy of the codes. A copy that differs would
		// accept a currency that the generated service refuses, or refuse one that it accepts.
		source, err := os.ReadFile(filepath.Join("..", "..", "registry", "runtime", "money", "money.go"))
		if err != nil {
			t.Fatal(err)
		}
		table := string(source[strings.Index(string(source), "var codes = map[string]int{"):])
		table = table[:strings.Index(table, "\n}\n")]
		runtime := map[string]bool{}
		for _, match := range regexp.MustCompile(`"([A-Z]{3})":`).FindAllStringSubmatch(table, -1) {
			runtime[match[1]] = true
		}
		if len(runtime) < 150 {
			t.Fatalf("read only %d codes from the runtime Money type", len(runtime))
		}
		for code := range runtime {
			if !currencyCodes[code] {
				t.Errorf("the runtime accepts %s, which checking refuses", code)
			}
		}
		for code := range currencyCodes {
			if !runtime[code] {
				t.Errorf("checking accepts %s, which the runtime refuses", code)
			}
		}
	})
}

func TestMoneyIsFloat(t *testing.T) {
	for _, c := range []struct {
		price string
		want  bool
	}{
		{"{name: price, type: float, currency: EUR}", true},
		{"{name: price, type: decimal, currency: EUR}", true},
		{"{name: price, type: money, currency: EUR}", false},
	} {
		t.Run("ST-DECL-SP09-T A money field typed as a float or a decimal satisfies it, and integer minor units with a currency do not: "+c.price, func(t *testing.T) {
			if got := MoneyIsFloat(td04Changed(t, withPrice(c.price)...)); got != c.want {
				t.Errorf("want %v, got %v", c.want, got)
			}
		})
	}
}

func TestLedgerCurrencyInvalid(t *testing.T) {
	{
		for _, c := range []struct {
			name   string
			change []string
			want   bool
		}{
			{"no currency", []string{"      currency: EUR\n", ""}, true},
			{"the currency EURO", []string{"currency: EUR\n", "currency: EURO\n"}, true},
			{"the minimum balance of customer_wallet in USD", []string{"{amount: 0, currency: EUR}", "{amount: 0, currency: USD}"}, true},
			{"ST-DECL-TD04 unchanged", nil, false},
		} {
			t.Run("ST-DECL-SP10-T A ledger with no currency, a non-ISO currency or a minimum balance in another currency satisfies it: "+c.name, func(t *testing.T) {
				if got := LedgerCurrencyInvalid(td04Changed(t, c.change...)); got != c.want {
					t.Errorf("want %v, got %v", c.want, got)
				}
			})
		}
	}
}

func TestLedgerCollectionMutates(t *testing.T) {
	{
		for _, c := range []struct {
			name   string
			change []string
			want   bool
		}{
			{"update added to transfers", []string{"reverse, read, list]", "reverse, read, list, update]"}, true},
			{"delete added to transfers", []string{"reverse, read, list]", "reverse, read, list, delete]"}, true},
			{"ST-DECL-TD04 unchanged", nil, false},
		} {
			t.Run("ST-DECL-SP11-T A ledger collection with update or delete satisfies it, and one with only the six ledger operations does not: "+c.name, func(t *testing.T) {
				if got := LedgerCollectionMutates(td04Changed(t, c.change...)); got != c.want {
					t.Errorf("want %v, got %v", c.want, got)
				}
			})
		}
	}
}
