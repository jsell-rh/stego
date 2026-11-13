package declaration

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jsell-rh/stego/internal/types"
)

// currencyCodes is the set of the ISO 4217 alphabetic codes that checking accepts: list one as of 2025, with the
// codes that the list keeps while a currency is replaced (ANG beside XCG, BGN), and without each code whose minor
// unit the list gives as N.A. (XXX, XTS, the metals, the bond market units, XDR, XSU and XUA). It is the set that
// the runtime Money type accepts (ST-SUP-RP08-LC12), so a declaration that checking accepts names no currency that
// the generated service refuses.
var currencyCodes = map[string]bool{
	"AED": true, "AFN": true, "ALL": true, "AMD": true, "ANG": true, "AOA": true, "ARS": true, "AUD": true, "AWG": true, "AZN": true,
	"BAM": true, "BBD": true, "BDT": true, "BGN": true, "BHD": true, "BIF": true, "BMD": true, "BND": true, "BOB": true, "BOV": true,
	"BRL": true, "BSD": true, "BTN": true, "BWP": true, "BYN": true, "BZD": true, "CAD": true, "CDF": true, "CHE": true, "CHF": true,
	"CHW": true, "CLF": true, "CLP": true, "CNY": true, "COP": true, "COU": true, "CRC": true, "CUP": true, "CVE": true, "CZK": true,
	"DJF": true, "DKK": true, "DOP": true, "DZD": true, "EGP": true, "ERN": true, "ETB": true, "EUR": true, "FJD": true, "FKP": true,
	"GBP": true, "GEL": true, "GHS": true, "GIP": true, "GMD": true, "GNF": true, "GTQ": true, "GYD": true, "HKD": true, "HNL": true,
	"HTG": true, "HUF": true, "IDR": true, "ILS": true, "INR": true, "IQD": true, "IRR": true, "ISK": true, "JMD": true, "JOD": true,
	"JPY": true, "KES": true, "KGS": true, "KHR": true, "KMF": true, "KPW": true, "KRW": true, "KWD": true, "KYD": true, "KZT": true,
	"LAK": true, "LBP": true, "LKR": true, "LRD": true, "LSL": true, "LYD": true, "MAD": true, "MDL": true, "MGA": true, "MKD": true,
	"MMK": true, "MNT": true, "MOP": true, "MRU": true, "MUR": true, "MVR": true, "MWK": true, "MXN": true, "MXV": true, "MYR": true,
	"MZN": true, "NAD": true, "NGN": true, "NIO": true, "NOK": true, "NPR": true, "NZD": true, "OMR": true, "PAB": true, "PEN": true,
	"PGK": true, "PHP": true, "PKR": true, "PLN": true, "PYG": true, "QAR": true, "RON": true, "RSD": true, "RUB": true, "RWF": true,
	"SAR": true, "SBD": true, "SCR": true, "SDG": true, "SEK": true, "SGD": true, "SHP": true, "SLE": true, "SOS": true, "SRD": true,
	"SSP": true, "STN": true, "SVC": true, "SYP": true, "SZL": true, "THB": true, "TJS": true, "TMT": true, "TND": true, "TOP": true,
	"TRY": true, "TTD": true, "TWD": true, "TZS": true, "UAH": true, "UGX": true, "USD": true, "USN": true, "UYI": true, "UYU": true,
	"UYW": true, "UZS": true, "VED": true, "VES": true, "VND": true, "VUV": true, "WST": true, "XAF": true, "XCD": true, "XCG": true,
	"XOF": true, "XPF": true, "YER": true, "ZAR": true, "ZMW": true, "ZWG": true,
}

// ledgerNameForm is the form of a transfer code, an account code and an account owner. The generated service writes
// the transfer codes into a check constraint and the account codes into its DDL, so the declaration holds them to a
// form that SQL cannot read as code (ST-DECL-INV86).
var ledgerNameForm = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

// The normal balances of an account (ST-DECL-EN15).
var normalBalances = []string{"debit", "credit"}

// floatTypes are the field types that hold a number with a fraction. A money field of one of them loses minor
// units, so checking refuses it (ST-DECL-SP09). decimal is not a field type of the format, but an author who means
// money may write it.
var floatTypes = []types.FieldType{types.FieldTypeFloat, types.FieldTypeDouble, "decimal"}

// isLedger tells whether the entity is of kind ledger.
func isLedger(e types.Entity) bool { return e.Kind == types.EntityKindLedger }

// ledgerEntities gives the names of the entities of kind ledger.
func ledgerEntities(entities []types.Entity) map[string]bool {
	ledgers := map[string]bool{}
	for _, e := range entities {
		if isLedger(e) {
			ledgers[e.Name] = true
		}
	}
	return ledgers
}

// MoneyIsFloat is the specification ST-DECL-SP09: a field that holds money, because it names a currency, is typed
// as a float or a decimal instead of integer minor units with a currency.
func MoneyIsFloat(service *types.ServiceDeclaration) bool {
	if service == nil {
		return false
	}
	for _, e := range service.Entities {
		for _, f := range e.Fields {
			if f.Currency != "" && slices.Contains(floatTypes, f.Type) {
				return true
			}
		}
	}
	return false
}

// LedgerCurrencyInvalid is the specification ST-DECL-SP10: a ledger has no currency, more than one, a currency that
// is not an ISO 4217 code, or an account minimum balance in another currency.
func LedgerCurrencyInvalid(service *types.ServiceDeclaration) bool {
	if service == nil {
		return false
	}
	for _, e := range service.Entities {
		if !isLedger(e) {
			continue
		}
		if e.Ledger == nil || len(e.Ledger.Currency) != 1 || !currencyCodes[e.Ledger.Currency[0]] {
			return true
		}
		for _, account := range e.Ledger.Accounts {
			if account.MinBalance != nil && account.MinBalance.Currency != e.Ledger.Currency[0] {
				return true
			}
		}
	}
	return false
}

// LedgerCollectionMutates is the specification ST-DECL-SP11: a collection over a ledger names update or delete
// among its operations.
func LedgerCollectionMutates(service *types.ServiceDeclaration) bool {
	if service == nil {
		return false
	}
	ledgers := ledgerEntities(service.Entities)
	for _, c := range service.Collections {
		if ledgers[c.Entity] && (slices.Contains(c.Operations, types.OpUpdate) || slices.Contains(c.Operations, types.OpDelete)) {
			return true
		}
	}
	return false
}

// validateMoney holds the declaration to ST-DECL-INV26: a money field is integer minor units with an ISO 4217
// currency, a ledger holds one ISO 4217 currency, its transfer codes and its two-phase rule, and each account
// states a normal balance of debit or credit, with a minimum balance as Money in the currency of the ledger. Each
// fault names the entity, and the field or the account, so that the author knows what to change.
func validateMoney(service *types.ServiceDeclaration) []ValidationError {
	var faults []ValidationError
	fault := func(rule, path, found string, allowed []string, format string, args ...any) {
		faults = append(faults, ValidationError{Category: "ledger", Rule: rule, Path: path, Found: found, Allowed: allowed, Message: fmt.Sprintf(format, args...)})
	}
	for ei, e := range service.Entities {
		for fi, f := range e.Fields {
			path := fieldPath(ei, fi)
			switch {
			case f.Type == types.FieldTypeMoney && f.Currency == "":
				fault("money", path+".currency", "", nil, "entity %q field %q is money with no currency; a money field is integer minor units with an ISO 4217 currency, such as currency: EUR", e.Name, f.Name)
			case f.Type == types.FieldTypeMoney && !currencyCodes[f.Currency]:
				fault("currency", path+".currency", f.Currency, nil, "entity %q field %q has the currency %q, which is not an ISO 4217 code, such as EUR", e.Name, f.Name, f.Currency)
			case f.Currency != "" && slices.Contains(floatTypes, f.Type):
				fault("money", path+".type", string(f.Type), []string{string(types.FieldTypeMoney)}, "entity %q field %q holds money as %s, which loses minor units; a money field is integer minor units: type money with its currency", e.Name, f.Name, f.Type)
			case f.Currency != "" && f.Type != types.FieldTypeMoney:
				fault("money", path+".type", string(f.Type), []string{string(types.FieldTypeMoney)}, "entity %q field %q names a currency, but only a field of type money holds a currency", e.Name, f.Name)
			}
		}

		switch {
		case e.Kind != "" && e.Kind != types.EntityKindData && !isLedger(e):
			fault("entity-kind", entityPath(ei)+".kind", e.Kind, []string{types.EntityKindData, types.EntityKindLedger}, "entity %q has the kind %q; the kind is data or ledger", e.Name, e.Kind)
			continue
		case !isLedger(e):
			if e.Ledger != nil {
				fault("ledger", entityPath(ei)+".ledger", "", nil, "entity %q gives ledger settings, but it is not of kind ledger; add kind: ledger or remove the ledger settings", e.Name)
			}
			continue
		}

		ledger := types.Ledger{}
		if e.Ledger != nil {
			ledger = *e.Ledger
		}
		path := entityPath(ei) + ".ledger"
		switch {
		case len(ledger.Currency) == 0:
			fault("ledger-currency", path+".currency", "", nil, "ledger %q names no currency; a ledger holds one ISO 4217 currency, such as currency: EUR", e.Name)
		case len(ledger.Currency) > 1:
			fault("ledger-currency", path+".currency", strings.Join(ledger.Currency, ", "), nil, "ledger %q names the currencies %s; a ledger holds one currency, so a move between currencies is two transfers through a conversion account", e.Name, strings.Join(ledger.Currency, " and "))
		case !currencyCodes[ledger.Currency[0]]:
			fault("ledger-currency", path+".currency", ledger.Currency[0], nil, "ledger %q has the currency %q, which is not an ISO 4217 code, such as EUR", e.Name, ledger.Currency[0])
		}
		if len(ledger.TransferCodes) == 0 {
			fault("ledger-transfer-codes", path+".transfer_codes", "", nil, "ledger %q names no transfer codes; a ledger names the codes of the transfers it accepts, such as transfer_codes: [topup, purchase]", e.Name)
		}
		seen := map[string]bool{}
		for i, code := range ledger.TransferCodes {
			at := fmt.Sprintf("%s.transfer_codes[%d]", path, i)
			if !ledgerNameForm.MatchString(code) {
				fault("name-form", at, code, []string{ledgerNameForm.String()}, "ledger %q has the transfer code %q, which does not match %s", e.Name, code, ledgerNameForm)
			} else if seen[code] {
				fault("ledger-transfer-codes", at, code, nil, "ledger %q names the transfer code %q more than once", e.Name, code)
			}
			seen[code] = true
		}
		if ledger.TwoPhase == nil {
			fault("ledger-two-phase", path+".two_phase", "", []string{"true", "false"}, "ledger %q does not state two phase; state two_phase: true when a transfer may be pended and then posted or voided, or false", e.Name)
		}
		if ledger.PendingTimeout != "" {
			if timeout, err := time.ParseDuration(ledger.PendingTimeout); err != nil || timeout <= 0 {
				fault("ledger-pending-timeout", path+".pending_timeout", ledger.PendingTimeout, nil, "ledger %q has the pending timeout %q, which is not a duration greater than zero, such as 15m", e.Name, ledger.PendingTimeout)
			}
		}
		if ledger.SnapshotEvery != nil && *ledger.SnapshotEvery < 1 {
			fault("ledger-snapshot", path+".snapshot_every", fmt.Sprint(*ledger.SnapshotEvery), nil, "ledger %q takes a snapshot every %d entries; the interval is a whole number of at least 1", e.Name, *ledger.SnapshotEvery)
		}
		accounts := map[string]bool{}
		for ai, account := range ledger.Accounts {
			at := fmt.Sprintf("%s.accounts[%d]", path, ai)
			if !ledgerNameForm.MatchString(account.Name) {
				fault("name-form", at+".name", account.Name, []string{ledgerNameForm.String()}, "ledger %q has the account %q, whose name does not match %s", e.Name, account.Name, ledgerNameForm)
			} else if accounts[account.Name] {
				fault("ledger-account", at+".name", account.Name, nil, "ledger %q has the account %q more than once", e.Name, account.Name)
			}
			accounts[account.Name] = true
			switch {
			case account.Normal == "":
				fault("normal-balance", at+".normal", "", normalBalances, "account %q of ledger %q states no normal balance; normal is debit or credit", account.Name, e.Name)
			case !slices.Contains(normalBalances, account.Normal):
				fault("normal-balance", at+".normal", account.Normal, normalBalances, "account %q of ledger %q has the normal balance %q; normal is debit or credit", account.Name, e.Name, account.Normal)
			}
			if account.Owner != "" && !ledgerNameForm.MatchString(account.Owner) {
				fault("name-form", at+".owner", account.Owner, []string{ledgerNameForm.String()}, "account %q of ledger %q has the owner %q, which does not match %s", account.Name, e.Name, account.Owner, ledgerNameForm)
			}
			if minimum := account.MinBalance; minimum != nil {
				switch {
				case !minimum.Amount.Whole:
					fault("money", at+".min_balance.amount", minimum.Amount.Text, nil, "account %q of ledger %q has the minimum balance %q, which is not Money: an amount is a whole number of minor units", account.Name, e.Name, minimum.Amount.Text)
				case minimum.Currency == "":
					fault("money", at+".min_balance.currency", "", nil, "account %q of ledger %q has a minimum balance with no currency, which is not Money; give the currency of the ledger", account.Name, e.Name)
				case len(ledger.Currency) == 1 && minimum.Currency != ledger.Currency[0]:
					fault("ledger-currency", at+".min_balance.currency", minimum.Currency, []string{ledger.Currency[0]}, "account %q of ledger %q has a minimum balance in %q; a ledger holds one currency, %s", account.Name, e.Name, minimum.Currency, ledger.Currency[0])
				}
			}
		}
	}
	return faults
}

// ledgerOperationNames gives the operations that a collection over a ledger may name, in the order of the format.
func ledgerOperationNames() []string {
	names := make([]string, 0, len(types.LedgerOperations))
	for _, op := range types.LedgerOperations {
		names = append(names, string(op))
	}
	return names
}

// validateLedgerCollections holds the declaration to ST-DECL-INV27: a collection over a ledger names only create,
// post, void, reverse, read and list, so it never updates or deletes a transfer or an entry. Each fault names the
// collection and the operation. The format holds no stored balance, so the strict input stage refuses a balance
// field of an account before this check runs.
func validateLedgerCollections(service *types.ServiceDeclaration) []ValidationError {
	ledgers := ledgerEntities(service.Entities)
	var faults []ValidationError
	for _, c := range service.Collections {
		if !ledgers[c.Entity] {
			continue
		}
		for oi, op := range c.Operations {
			if slices.Contains(types.LedgerOperations, op) {
				continue
			}
			faults = append(faults, ValidationError{
				Category: "operation", Rule: "ledger-operation", Path: fmt.Sprintf("%s.operations[%d]", collectionPath(c.Name), oi), Found: string(op), Allowed: ledgerOperationNames(),
				Message: fmt.Sprintf("collection %q over the ledger %q allows %q; a ledger is append-only, so its collection allows only create, post, void, reverse, read and list, and a correction is a reverse", c.Name, c.Entity, op),
			})
		}
	}
	return faults
}

// accountKey matches the path of a key of one ledger account, such as entities[1].ledger.accounts[0].balance.
var accountKey = regexp.MustCompile(`^entities\[(\d+)\]\.ledger\.accounts\[(\d+)\]\.([^.\[]+)$`)

// nameLedgerAccounts makes each fault of a key of a ledger account that the format does not declare name the account
// and its ledger, since the path gives only the place of the account in the list. A balance key also gets the
// reason: a balance is derived from the entries, so the format holds no stored balance (ST-DECL-INV27).
func nameLedgerAccounts(service *types.ServiceDeclaration, faults []ValidationError) {
	for i, fault := range faults {
		match := accountKey.FindStringSubmatch(fault.Path)
		if match == nil {
			continue
		}
		ei, _ := strconv.Atoi(match[1])
		ai, _ := strconv.Atoi(match[2])
		if ei >= len(service.Entities) || service.Entities[ei].Ledger == nil || ai >= len(service.Entities[ei].Ledger.Accounts) {
			continue
		}
		entity := service.Entities[ei]
		message := fmt.Sprintf("field %q of the account %q of ledger %q is a field the declaration format does not hold", match[3], entity.Ledger.Accounts[ai].Name, entity.Name)
		if match[3] == "balance" {
			message += "; a balance is derived from the latest snapshot and the entries after it, so no declaration states a stored balance"
		}
		faults[i].Message = message
	}
}
