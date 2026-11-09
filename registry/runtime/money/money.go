package money

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"slices"
)

var (
	// ErrCurrency is the error of New and Parse for a code that is not an ISO 4217 currency code.
	ErrCurrency = errors.New("money: not an ISO 4217 currency code")
	// ErrCurrencyMismatch is the error of Add and Sub when the two amounts have different currencies.
	ErrCurrencyMismatch = errors.New("money: the currencies differ")
	// ErrOverflow is the error of Add and Sub when the result does not fit in an int64 of minor units.
	ErrOverflow = errors.New("money: the result overflows int64 minor units")
	// ErrRatios is the error of Allocate for an empty, all-zero or negative ratio set, or one whose total does not
	// fit in 64 bits.
	ErrRatios = errors.New("money: the ratios cannot split an amount")
)

// Currency is an ISO 4217 alphabetic currency code, such as USD. Only Parse makes a currency that is not zero.
type Currency struct{ code string }

// Code is the three-letter code, or "" for the zero currency.
func (c Currency) Code() string { return c.code }

// String is the code.
func (c Currency) String() string { return c.code }

// Exponent is the ISO 4217 minor-unit exponent of the currency: the number of decimal digits of its minor unit,
// such as 0 for JPY, 2 for USD and 3 for BHD (ST-SUP-RP08-LC12). It is -1 for the zero currency.
func (c Currency) Exponent() int {
	if e, ok := codes[c.code]; ok {
		return e
	}
	return -1
}

// IsZero reports whether c is the zero currency, which no Money holds.
func (c Currency) IsZero() bool { return c.code == "" }

// Parse returns the currency of an ISO 4217 alphabetic code. The code is case-sensitive: usd is refused, as is a
// code that ISO 4217 does not list, such as ZZZ, and each code with no minor unit: XXX, the test code XTS, the
// metals XAU, XAG, XPT and XPD, and the units XBA to XBD, XDR, XSU and XUA.
func Parse(code string) (Currency, error) {
	if _, ok := codes[code]; !ok {
		return Currency{}, fmt.Errorf("%w: %q", ErrCurrency, code)
	}
	return Currency{code: code}, nil
}

// Money is an amount in int64 minor units, such as cents, with its currency. A Money value never changes; Add, Sub
// and Allocate return new values. The zero Money has no currency, and Add and Sub refuse it.
type Money struct {
	amount   int64
	currency Currency
}

// New returns the amount of minor units in the currency of the ISO 4217 code.
func New(amount int64, code string) (Money, error) {
	c, err := Parse(code)
	if err != nil {
		return Money{}, err
	}
	return Money{amount: amount, currency: c}, nil
}

// Of returns the amount of minor units in the currency c. It refuses the zero currency.
func Of(amount int64, c Currency) (Money, error) {
	if c.IsZero() {
		return Money{}, fmt.Errorf("%w: the zero currency", ErrCurrency)
	}
	return Money{amount: amount, currency: c}, nil
}

// Amount is the amount in minor units.
func (m Money) Amount() int64 { return m.amount }

// Currency is the currency of the amount.
func (m Money) Currency() Currency { return m.currency }

// IsZero reports whether m is the zero Money, which has no currency.
func (m Money) IsZero() bool { return m.currency.IsZero() && m.amount == 0 }

// String is the amount in minor units and the code, such as "1050 USD".
func (m Money) String() string { return fmt.Sprintf("%d %s", m.amount, m.currency.code) }

// Add returns m + o. It returns an error that names both currencies when they differ, and ErrOverflow when the sum
// does not fit in an int64; it never returns a wrapped value.
func (m Money) Add(o Money) (Money, error) {
	if err := same(m, o); err != nil {
		return Money{}, err
	}
	sum := m.amount + o.amount
	if (o.amount > 0 && sum < m.amount) || (o.amount < 0 && sum > m.amount) {
		return Money{}, fmt.Errorf("%w: %d + %d %s", ErrOverflow, m.amount, o.amount, m.currency.code)
	}
	return Money{amount: sum, currency: m.currency}, nil
}

// Sub returns m - o. It returns an error that names both currencies when they differ, and ErrOverflow when the
// difference does not fit in an int64; it never returns a wrapped value.
func (m Money) Sub(o Money) (Money, error) {
	if err := same(m, o); err != nil {
		return Money{}, err
	}
	diff := m.amount - o.amount
	if (o.amount > 0 && diff > m.amount) || (o.amount < 0 && diff < m.amount) {
		return Money{}, fmt.Errorf("%w: %d - %d %s", ErrOverflow, m.amount, o.amount, m.currency.code)
	}
	return Money{amount: diff, currency: m.currency}, nil
}

func same(m, o Money) error {
	if m.currency.IsZero() || o.currency.IsZero() {
		return fmt.Errorf("%w: an amount has no currency (%q and %q)", ErrCurrency, m.currency.code, o.currency.code)
	}
	if m.currency != o.currency {
		return fmt.Errorf("%w: %s and %s", ErrCurrencyMismatch, m.currency.code, o.currency.code)
	}
	return nil
}

// Allocate splits m by the ratios into one part for each ratio, in the order of the ratios, so that the parts sum
// to m exactly and no minor unit is lost. Each part first gets the whole minor units of its exact share, rounded
// toward zero; the minor units left over then go one at a time to the parts with the largest fractional share,
// and between equal fractional shares to the part with the lower index. So the result depends only on the amount
// and the ratios, every part is within one minor unit of its exact share, and a part of ratio 0 gets 0. A negative
// amount splits as its magnitude, with each part negated. Allocate refuses an empty or all-zero ratio set, a
// negative ratio, and ratios whose total does not fit in 64 bits.
func (m Money) Allocate(ratios ...int64) ([]Money, error) {
	if m.currency.IsZero() {
		return nil, fmt.Errorf("%w: the amount has no currency", ErrCurrency)
	}
	if len(ratios) == 0 {
		return nil, fmt.Errorf("%w: no ratios", ErrRatios)
	}
	var total uint64
	for i, r := range ratios {
		if r < 0 {
			return nil, fmt.Errorf("%w: the ratio %d at index %d is negative", ErrRatios, r, i)
		}
		var carry uint64
		total, carry = bits.Add64(total, uint64(r), 0)
		if carry != 0 {
			return nil, fmt.Errorf("%w: the total of the ratios overflows 64 bits", ErrRatios)
		}
	}
	if total == 0 {
		return nil, fmt.Errorf("%w: every ratio is zero", ErrRatios)
	}
	negative := m.amount < 0
	// magnitude is |amount|; for math.MinInt64 it is 2^63, which fits in a uint64.
	magnitude := uint64(m.amount)
	if negative {
		magnitude = -magnitude
	}
	type share struct {
		index int
		whole uint64
		rest  uint64 // the fractional share, in units of 1/total
	}
	shares := make([]share, len(ratios))
	var given uint64
	for i, r := range ratios {
		// magnitude*r < 2^64*total because r <= total, so the high word is below total and Div64 cannot panic.
		hi, lo := bits.Mul64(magnitude, uint64(r))
		q, rem := bits.Div64(hi, lo, total)
		shares[i] = share{index: i, whole: q, rest: rem}
		given += q
	}
	// The left-over units are fewer than the parts: the fractional shares sum to left*total, and each is below
	// total.
	left := magnitude - given
	order := slices.Clone(shares)
	slices.SortFunc(order, func(a, b share) int {
		if c := cmp.Compare(b.rest, a.rest); c != 0 {
			return c
		}
		return cmp.Compare(a.index, b.index)
	})
	for k := uint64(0); k < left; k++ {
		shares[order[k].index].whole++
	}
	parts := make([]Money, len(ratios))
	for i, s := range shares {
		parts[i] = Money{amount: signed(s.whole, negative), currency: m.currency}
	}
	return parts, nil
}

// signed returns u, or -u when negative is set. u is at most 2^63, and only when negative is set.
func signed(u uint64, negative bool) int64 {
	if !negative {
		return int64(u)
	}
	if u == 1<<63 {
		return math.MinInt64
	}
	return -int64(u)
}

// codes maps each accepted alphabetic code of ISO 4217 list one (current currency and funds codes) as of 2025 to its
// minor-unit exponent from that list, with the codes that the list keeps while a currency is replaced (ANG beside
// XCG, BGN). It leaves out each code whose minor unit the list gives as N.A.: XXX (no currency), XTS (testing),
// the metals XAU, XAG, XPT and XPD, the bond market units XBA, XBB, XBC and XBD, and XDR, XSU and XUA
// (ST-SUP-RP08-LC12).
var codes = map[string]int{
	"AED": 2, "AFN": 2, "ALL": 2, "AMD": 2, "ANG": 2, "AOA": 2, "ARS": 2, "AUD": 2, "AWG": 2, "AZN": 2,
	"BAM": 2, "BBD": 2, "BDT": 2, "BGN": 2, "BHD": 3, "BIF": 0, "BMD": 2, "BND": 2, "BOB": 2, "BOV": 2,
	"BRL": 2, "BSD": 2, "BTN": 2, "BWP": 2, "BYN": 2, "BZD": 2, "CAD": 2, "CDF": 2, "CHE": 2, "CHF": 2,
	"CHW": 2, "CLF": 4, "CLP": 0, "CNY": 2, "COP": 2, "COU": 2, "CRC": 2, "CUP": 2, "CVE": 2, "CZK": 2,
	"DJF": 0, "DKK": 2, "DOP": 2, "DZD": 2, "EGP": 2, "ERN": 2, "ETB": 2, "EUR": 2, "FJD": 2, "FKP": 2,
	"GBP": 2, "GEL": 2, "GHS": 2, "GIP": 2, "GMD": 2, "GNF": 0, "GTQ": 2, "GYD": 2, "HKD": 2, "HNL": 2,
	"HTG": 2, "HUF": 2, "IDR": 2, "ILS": 2, "INR": 2, "IQD": 3, "IRR": 2, "ISK": 0, "JMD": 2, "JOD": 3,
	"JPY": 0, "KES": 2, "KGS": 2, "KHR": 2, "KMF": 0, "KPW": 2, "KRW": 0, "KWD": 3, "KYD": 2, "KZT": 2,
	"LAK": 2, "LBP": 2, "LKR": 2, "LRD": 2, "LSL": 2, "LYD": 3, "MAD": 2, "MDL": 2, "MGA": 2, "MKD": 2,
	"MMK": 2, "MNT": 2, "MOP": 2, "MRU": 2, "MUR": 2, "MVR": 2, "MWK": 2, "MXN": 2, "MXV": 2, "MYR": 2,
	"MZN": 2, "NAD": 2, "NGN": 2, "NIO": 2, "NOK": 2, "NPR": 2, "NZD": 2, "OMR": 3, "PAB": 2, "PEN": 2,
	"PGK": 2, "PHP": 2, "PKR": 2, "PLN": 2, "PYG": 0, "QAR": 2, "RON": 2, "RSD": 2, "RUB": 2, "RWF": 0,
	"SAR": 2, "SBD": 2, "SCR": 2, "SDG": 2, "SEK": 2, "SGD": 2, "SHP": 2, "SLE": 2, "SOS": 2, "SRD": 2,
	"SSP": 2, "STN": 2, "SVC": 2, "SYP": 2, "SZL": 2, "THB": 2, "TJS": 2, "TMT": 2, "TND": 3, "TOP": 2,
	"TRY": 2, "TTD": 2, "TWD": 2, "TZS": 2, "UAH": 2, "UGX": 0, "USD": 2, "USN": 2, "UYI": 0, "UYU": 2,
	"UYW": 4, "UZS": 2, "VED": 2, "VES": 2, "VND": 0, "VUV": 0, "WST": 2, "XAF": 0, "XCD": 2, "XCG": 2,
	"XOF": 0, "XPF": 0, "YER": 2, "ZAR": 2, "ZMW": 2, "ZWG": 2,
}
