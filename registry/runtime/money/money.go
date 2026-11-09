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

// IsZero reports whether c is the zero currency, which no Money holds.
func (c Currency) IsZero() bool { return c.code == "" }

// Parse returns the currency of an ISO 4217 alphabetic code. The code is case-sensitive: usd is refused, as is a
// code that ISO 4217 does not list, such as ZZZ, and XXX, the code of a transaction with no currency.
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

// codes is the alphabetic codes of ISO 4217 list one (current currency and funds codes) as of 2025, with the codes
// that list keeps while a currency is replaced (ANG beside XCG, BGN). It leaves out XXX, the code of a transaction
// with no currency, which no amount can carry.
var codes = map[string]struct{}{
	"AED": {}, "AFN": {}, "ALL": {}, "AMD": {}, "ANG": {}, "AOA": {}, "ARS": {}, "AUD": {}, "AWG": {}, "AZN": {},
	"BAM": {}, "BBD": {}, "BDT": {}, "BGN": {}, "BHD": {}, "BIF": {}, "BMD": {}, "BND": {}, "BOB": {}, "BOV": {},
	"BRL": {}, "BSD": {}, "BTN": {}, "BWP": {}, "BYN": {}, "BZD": {}, "CAD": {}, "CDF": {}, "CHE": {}, "CHF": {},
	"CHW": {}, "CLF": {}, "CLP": {}, "CNY": {}, "COP": {}, "COU": {}, "CRC": {}, "CUP": {}, "CVE": {}, "CZK": {},
	"DJF": {}, "DKK": {}, "DOP": {}, "DZD": {}, "EGP": {}, "ERN": {}, "ETB": {}, "EUR": {}, "FJD": {}, "FKP": {},
	"GBP": {}, "GEL": {}, "GHS": {}, "GIP": {}, "GMD": {}, "GNF": {}, "GTQ": {}, "GYD": {}, "HKD": {}, "HNL": {},
	"HTG": {}, "HUF": {}, "IDR": {}, "ILS": {}, "INR": {}, "IQD": {}, "IRR": {}, "ISK": {}, "JMD": {}, "JOD": {},
	"JPY": {}, "KES": {}, "KGS": {}, "KHR": {}, "KMF": {}, "KPW": {}, "KRW": {}, "KWD": {}, "KYD": {}, "KZT": {},
	"LAK": {}, "LBP": {}, "LKR": {}, "LRD": {}, "LSL": {}, "LYD": {}, "MAD": {}, "MDL": {}, "MGA": {}, "MKD": {},
	"MMK": {}, "MNT": {}, "MOP": {}, "MRU": {}, "MUR": {}, "MVR": {}, "MWK": {}, "MXN": {}, "MXV": {}, "MYR": {},
	"MZN": {}, "NAD": {}, "NGN": {}, "NIO": {}, "NOK": {}, "NPR": {}, "NZD": {}, "OMR": {}, "PAB": {}, "PEN": {},
	"PGK": {}, "PHP": {}, "PKR": {}, "PLN": {}, "PYG": {}, "QAR": {}, "RON": {}, "RSD": {}, "RUB": {}, "RWF": {},
	"SAR": {}, "SBD": {}, "SCR": {}, "SDG": {}, "SEK": {}, "SGD": {}, "SHP": {}, "SLE": {}, "SOS": {}, "SRD": {},
	"SSP": {}, "STN": {}, "SVC": {}, "SYP": {}, "SZL": {}, "THB": {}, "TJS": {}, "TMT": {}, "TND": {}, "TOP": {},
	"TRY": {}, "TTD": {}, "TWD": {}, "TZS": {}, "UAH": {}, "UGX": {}, "USD": {}, "USN": {}, "UYI": {}, "UYU": {},
	"UYW": {}, "UZS": {}, "VED": {}, "VES": {}, "VND": {}, "VUV": {}, "WST": {}, "XAF": {}, "XAG": {}, "XAU": {},
	"XBA": {}, "XBB": {}, "XBC": {}, "XBD": {}, "XCD": {}, "XCG": {}, "XDR": {}, "XOF": {}, "XPD": {}, "XPF": {},
	"XPT": {}, "XSU": {}, "XTS": {}, "XUA": {}, "YER": {}, "ZAR": {}, "ZMW": {}, "ZWG": {},
}
