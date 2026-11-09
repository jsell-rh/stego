package money

import (
	"errors"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"math"
	"math/big"
	"math/rand/v2"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func must(t *testing.T, amount int64, code string) Money {
	t.Helper()
	m, err := New(amount, code)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// edges are the int64 values where overflow and rounding go wrong first.
var edges = []int64{math.MinInt64, math.MinInt64 + 1, math.MinInt64 + 2, -1 << 32, -101, -100, -3, -2, -1, 0, 1, 2, 3,
	100, 101, 1 << 32, math.MaxInt64 - 2, math.MaxInt64 - 1, math.MaxInt64}

func TestMoney(t *testing.T) {
	t.Run("ST-SUP-RP08-LC01-T01 Money holds int64 minor units and an ISO 4217 code", func(t *testing.T) {
		for _, code := range []string{"USD", "JPY"} {
			m, err := New(1050, code)
			if err != nil {
				t.Fatalf("New(1050, %s): %v", code, err)
			}
			if m.Currency().Code() != code || m.Amount() != 1050 {
				t.Fatalf("New(1050, %s) = %v", code, m)
			}
		}
		for _, code := range []string{"usd", "ZZZ", "XXX", "", "US", "USDD"} {
			m, err := New(1, code)
			if !errors.Is(err, ErrCurrency) {
				t.Fatalf("New(1, %q) = %v, %v; want ErrCurrency", code, m, err)
			}
			if !strings.Contains(err.Error(), `"`+code+`"`) {
				t.Fatalf("the error %q does not name the code %q", err, code)
			}
			if !m.IsZero() {
				t.Fatalf("New(1, %q) returned the value %v with its error", code, m)
			}
		}
		if _, err := Of(1, Currency{}); !errors.Is(err, ErrCurrency) {
			t.Fatalf("Of with the zero currency: %v; want ErrCurrency", err)
		}
		// The amount is of type int64, in the struct and in the Amount method.
		pkg := typeCheck(t)
		money := pkg.Scope().Lookup("Money").Type().(*types.Named)
		fields := money.Underlying().(*types.Struct)
		var amount types.Type
		for i := range fields.NumFields() {
			if fields.Field(i).Name() == "amount" {
				amount = fields.Field(i).Type()
			}
		}
		if amount == nil || !types.Identical(amount, types.Typ[types.Int64]) {
			t.Fatalf("the amount field of Money is %v, not int64", amount)
		}
		for i := range money.NumMethods() {
			if m := money.Method(i); m.Name() == "Amount" {
				res := m.Type().(*types.Signature).Results()
				if res.Len() != 1 || !types.Identical(res.At(0).Type(), types.Typ[types.Int64]) {
					t.Fatalf("Money.Amount returns %v, not int64", res)
				}
			}
		}
	})

	t.Run("ST-SUP-RP08-LC01-T02 Add and Sub refuse two currencies", func(t *testing.T) {
		usd, eur := must(t, 100, "USD"), must(t, 100, "EUR")
		for name, op := range map[string]func(Money, Money) (Money, error){"Add": Money.Add, "Sub": Money.Sub} {
			got, err := op(usd, eur)
			if !errors.Is(err, ErrCurrencyMismatch) {
				t.Fatalf("%s(100 USD, 100 EUR) = %v, %v; want ErrCurrencyMismatch", name, got, err)
			}
			if !strings.Contains(err.Error(), "USD") || !strings.Contains(err.Error(), "EUR") {
				t.Fatalf("the error of %s %q does not name both currencies", name, err)
			}
			if !got.IsZero() {
				t.Fatalf("%s returned the value %v with its error", name, got)
			}
			if _, err := op(usd, Money{}); !errors.Is(err, ErrCurrency) {
				t.Fatalf("%s with the zero Money: %v; want ErrCurrency", name, err)
			}
		}
	})

	t.Run("ST-SUP-RP08-LC01-T03 Add and Sub refuse an overflow", func(t *testing.T) {
		if got, err := must(t, math.MaxInt64, "USD").Add(must(t, 1, "USD")); !errors.Is(err, ErrOverflow) || !got.IsZero() {
			t.Fatalf("MaxInt64 + 1 = %v, %v; want ErrOverflow and no value", got, err)
		}
		if got, err := must(t, math.MinInt64, "USD").Sub(must(t, 1, "USD")); !errors.Is(err, ErrOverflow) || !got.IsZero() {
			t.Fatalf("MinInt64 - 1 = %v, %v; want ErrOverflow and no value", got, err)
		}
		// Every pair of edge values, and random pairs, against exact arithmetic: a result that fits is exact, and
		// one that does not is ErrOverflow.
		pairs := make([][2]int64, 0, len(edges)*len(edges)+20000)
		for _, a := range edges {
			for _, b := range edges {
				pairs = append(pairs, [2]int64{a, b})
			}
		}
		rng := rand.New(rand.NewPCG(1, 2))
		for range 20000 {
			pairs = append(pairs, [2]int64{int64(rng.Uint64()), int64(rng.Uint64())})
		}
		lo, hi := big.NewInt(math.MinInt64), big.NewInt(math.MaxInt64)
		for _, p := range pairs {
			a, b := must(t, p[0], "EUR"), must(t, p[1], "EUR")
			for _, c := range []struct {
				name  string
				op    func(Money, Money) (Money, error)
				exact *big.Int
			}{
				{"+", Money.Add, new(big.Int).Add(big.NewInt(p[0]), big.NewInt(p[1]))},
				{"-", Money.Sub, new(big.Int).Sub(big.NewInt(p[0]), big.NewInt(p[1]))},
			} {
				got, err := c.op(a, b)
				fits := c.exact.Cmp(lo) >= 0 && c.exact.Cmp(hi) <= 0
				switch {
				case fits && err != nil:
					t.Fatalf("%d %s %d: %v; want %s", p[0], c.name, p[1], err, c.exact)
				case fits && (got.Amount() != c.exact.Int64() || got.Currency().Code() != "EUR"):
					t.Fatalf("%d %s %d = %v; want %s EUR", p[0], c.name, p[1], got, c.exact)
				case !fits && !errors.Is(err, ErrOverflow):
					t.Fatalf("%d %s %d = %v, %v; want ErrOverflow", p[0], c.name, p[1], got, err)
				}
			}
		}
	})

	t.Run("ST-SUP-RP08-LC01-T04 The money package has no float conversion", func(t *testing.T) {
		pkg := typeCheck(t)
		var checked int
		check := func(where string, sig *types.Signature) {
			checked++
			for _, tuple := range []*types.Tuple{sig.Params(), sig.Results()} {
				for i := range tuple.Len() {
					if hasFloat(tuple.At(i).Type()) {
						t.Errorf("%s has the float parameter or result %v", where, tuple.At(i).Type())
					}
				}
			}
		}
		scope := pkg.Scope()
		for _, name := range scope.Names() {
			obj := scope.Lookup(name)
			if !obj.Exported() {
				continue
			}
			if fn, ok := obj.(*types.Func); ok {
				check(name, fn.Type().(*types.Signature))
			}
			if tn, ok := obj.(*types.TypeName); ok {
				if named, ok := tn.Type().(*types.Named); ok {
					for i := range named.NumMethods() {
						if m := named.Method(i); m.Exported() {
							check(name+"."+m.Name(), m.Type().(*types.Signature))
						}
					}
				}
			}
		}
		if checked < 8 {
			t.Fatalf("only %d exported functions and methods checked", checked)
		}
	})
}

func TestCurrencyExponent(t *testing.T) {
	t.Run("ST-SUP-RP08-LC12-T01 Test and metal codes are refused, and each code has its exponent", func(t *testing.T) {
		for _, code := range []string{"XTS", "XAU", "XAG", "XPT", "XPD", "XXX", "XBA", "XDR"} {
			if c, err := Parse(code); !errors.Is(err, ErrCurrency) || !c.IsZero() {
				t.Fatalf("Parse(%s) = %v, %v; want ErrCurrency", code, c, err)
			}
		}
		for code, want := range map[string]int{"JPY": 0, "USD": 2, "BHD": 3, "EUR": 2, "KRW": 0, "KWD": 3, "CLF": 4, "XOF": 0} {
			c, err := Parse(code)
			if err != nil {
				t.Fatalf("Parse(%s): %v", code, err)
			}
			if got := c.Exponent(); got != want {
				t.Fatalf("the exponent of %s is %d; want %d", code, got, want)
			}
		}
		if got := (Currency{}).Exponent(); got != -1 {
			t.Fatalf("the exponent of the zero currency is %d", got)
		}
		// Every code against the fixed table of ISO 4217 list one: the same codes, each with its minor unit.
		for code, want := range iso4217 {
			c, err := Parse(code)
			if err != nil {
				t.Fatalf("Parse(%s): %v", code, err)
			}
			if got := c.Exponent(); got != want {
				t.Fatalf("the exponent of %s is %d; ISO 4217 gives %d", code, got, want)
			}
		}
		for code := range codes {
			if _, ok := iso4217[code]; !ok {
				t.Fatalf("the code %s is accepted and is not in the table", code)
			}
		}
	})
}

func hasFloat(typ types.Type) bool {
	switch u := typ.Underlying().(type) {
	case *types.Basic:
		return u.Info()&(types.IsFloat|types.IsComplex) != 0
	case *types.Slice:
		return hasFloat(u.Elem())
	case *types.Array:
		return hasFloat(u.Elem())
	case *types.Pointer:
		return hasFloat(u.Elem())
	case *types.Map:
		return hasFloat(u.Key()) || hasFloat(u.Elem())
	}
	return false
}

func TestAllocate(t *testing.T) {
	t.Run("ST-SUP-RP08-LC02-T01 The allocated parts sum to the amount exactly", func(t *testing.T) {
		rng := rand.New(rand.NewPCG(3, 4))
		ratioSets := [][]int64{{1}, {1, 1}, {1, 1, 1}, {1, 2}, {2, 1}, {0, 1}, {1, 0, 0}, {0, 0, 7}, {3, 7, 11, 13},
			{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1}, {math.MaxInt64}, {math.MaxInt64, 1}, {1, math.MaxInt64 - 1},
			{math.MaxInt64 / 3, math.MaxInt64 / 3, math.MaxInt64 / 3}}
		for range 8 {
			set := make([]int64, 40+rng.IntN(60))
			for i := range set {
				set[i] = 1 + rng.Int64N(3)
			}
			ratioSets = append(ratioSets, set)
		}
		for range 24 {
			n := 1 + rng.IntN(12)
			set := make([]int64, n)
			for i := range set {
				set[i] = rng.Int64N(1000)
			}
			set[rng.IntN(n)]++ // never all zero
			ratioSets = append(ratioSets, set)
		}
		cases := 0
		for amount := int64(-10000); amount <= 10000; amount++ {
			for _, ratios := range ratioSets {
				checkAllocation(t, amount, ratios)
				cases++
			}
		}
		for _, amount := range edges {
			for _, ratios := range ratioSets {
				checkAllocation(t, amount, ratios)
				cases++
			}
		}
		for range 20000 {
			n := 1 + rng.IntN(8)
			set := make([]int64, n)
			for i := range set {
				set[i] = int64(rng.Uint64() >> (1 + rng.IntN(63)))
			}
			set[0]++
			if _, err := must(t, 0, "USD").Allocate(set...); err != nil {
				continue // a total over 64 bits, which ErrRatios refuses
			}
			checkAllocation(t, int64(rng.Uint64()), set)
			cases++
		}
		t.Logf("%d allocations checked", cases)
	})

	t.Run("ST-SUP-RP08-LC02-T02 The remainder goes one minor unit at a time in a fixed order", func(t *testing.T) {
		for _, c := range []struct {
			amount int64
			want   []int64
		}{{100, []int64{34, 33, 33}}, {101, []int64{34, 34, 33}}, {-100, []int64{-34, -33, -33}}} {
			var first []int64
			for call := range 2 {
				parts, err := must(t, c.amount, "USD").Allocate(1, 1, 1)
				if err != nil {
					t.Fatal(err)
				}
				got := amounts(parts)
				if call == 0 {
					first = got
				}
				if !equal(got, first) || !equal(got, c.want) {
					t.Fatalf("call %d: Allocate(%d, 1, 1, 1) = %v; want %v, the same on each call", call+1, c.amount, got, c.want)
				}
			}
		}
		// Between equal fractional shares the lower index comes first, also past the size where a sort is stable by
		// chance: 100 by 17 equal ratios is 5 each and 15 units left over, for the first 15 parts.
		ones := make([]int64, 17)
		want := make([]int64, 17)
		for i := range ones {
			ones[i], want[i] = 1, 5
			if i < 15 {
				want[i] = 6
			}
		}
		for range 2 {
			parts, err := must(t, 100, "USD").Allocate(ones...)
			if err != nil {
				t.Fatal(err)
			}
			if got := amounts(parts); !equal(got, want) {
				t.Fatalf("Allocate(100, 17 x 1) = %v; want %v", got, want)
			}
		}
		// The units left over go to the largest fractional shares: 10 by 1, 2, 3 is 1.67, 3.33, 5 exactly.
		parts, err := must(t, 10, "USD").Allocate(1, 2, 3)
		if err != nil {
			t.Fatal(err)
		}
		if got := amounts(parts); !equal(got, []int64{2, 3, 5}) {
			t.Fatalf("Allocate(10, 1, 2, 3) = %v; want [2 3 5]", got)
		}
	})

	t.Run("ST-SUP-RP08-LC02-T03 An empty or all-zero ratio set is refused", func(t *testing.T) {
		for _, ratios := range [][]int64{nil, {}, {0, 0}, {0}, {1, -1}, {-1}, {0, -5}, {math.MinInt64}, {math.MaxInt64, math.MaxInt64, 2}} {
			parts, err := must(t, 100, "USD").Allocate(ratios...)
			if !errors.Is(err, ErrRatios) || parts != nil {
				t.Fatalf("Allocate(100, %v) = %v, %v; want ErrRatios and no parts", ratios, parts, err)
			}
		}
		if parts, err := (Money{}).Allocate(1); err == nil || parts != nil {
			t.Fatalf("Allocate of the zero Money = %v, %v; want an error and no parts", parts, err)
		}
	})
}

// checkAllocation checks one allocation against exact arithmetic: one part per ratio, in the currency of the
// amount, summing to the amount exactly, each part within one minor unit of its exact share and on the side of
// the amount's sign, a part of ratio 0 equal to 0, and the same parts on a second call.
func checkAllocation(t *testing.T, amount int64, ratios []int64) {
	t.Helper()
	m := must(t, amount, "EUR")
	parts, err := m.Allocate(ratios...)
	if err != nil {
		t.Fatalf("Allocate(%d, %v): %v", amount, ratios, err)
	}
	if len(parts) != len(ratios) {
		t.Fatalf("Allocate(%d, %v) gave %d parts", amount, ratios, len(parts))
	}
	total := new(big.Int)
	for _, r := range ratios {
		total.Add(total, big.NewInt(r))
	}
	sum := new(big.Int)
	for i, p := range parts {
		if p.Currency().Code() != "EUR" {
			t.Fatalf("Allocate(%d, %v): part %d is in %q", amount, ratios, i, p.Currency().Code())
		}
		sum.Add(sum, big.NewInt(p.Amount()))
		// |part*total - amount*ratio| < total, so the part is within one unit of the exact share.
		diff := new(big.Int).Mul(big.NewInt(p.Amount()), total)
		diff.Sub(diff, new(big.Int).Mul(big.NewInt(amount), big.NewInt(ratios[i])))
		if diff.CmpAbs(total) >= 0 {
			t.Fatalf("Allocate(%d, %v): part %d is %d, more than one unit from its share", amount, ratios, i, p.Amount())
		}
		if (amount >= 0 && p.Amount() < 0) || (amount <= 0 && p.Amount() > 0) || (ratios[i] == 0 && p.Amount() != 0) {
			t.Fatalf("Allocate(%d, %v): part %d is %d", amount, ratios, i, p.Amount())
		}
	}
	if sum.Cmp(big.NewInt(amount)) != 0 {
		t.Fatalf("Allocate(%d, %v) = %v sums to %s", amount, ratios, amounts(parts), sum)
	}
	if want := reference(amount, ratios); !equal(amounts(parts), want) {
		t.Fatalf("Allocate(%d, %v) = %v; the largest-remainder order with ties to the lower index gives %v", amount, ratios, amounts(parts), want)
	}
	again, _ := m.Allocate(ratios...)
	if !equal(amounts(again), amounts(parts)) {
		t.Fatalf("Allocate(%d, %v) gave %v, then %v", amount, ratios, amounts(parts), amounts(again))
	}
}

// reference is an independent allocation in exact arithmetic: the whole share of each part rounded toward zero,
// then one unit each to the parts with the largest fractional share, ties to the lower index (a stable sort).
func reference(amount int64, ratios []int64) []int64 {
	total := new(big.Int)
	for _, r := range ratios {
		total.Add(total, big.NewInt(r))
	}
	magnitude := new(big.Int).Abs(big.NewInt(amount))
	whole := make([]*big.Int, len(ratios))
	rest := make([]*big.Int, len(ratios))
	given := new(big.Int)
	for i, r := range ratios {
		whole[i], rest[i] = new(big.Int).QuoRem(new(big.Int).Mul(magnitude, big.NewInt(r)), total, new(big.Int))
		given.Add(given, whole[i])
	}
	order := make([]int, len(ratios))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return rest[order[a]].Cmp(rest[order[b]]) > 0 })
	left := new(big.Int).Sub(magnitude, given).Int64()
	for k := range left {
		whole[order[k]].Add(whole[order[k]], big.NewInt(1))
	}
	out := make([]int64, len(ratios))
	for i, w := range whole {
		if amount < 0 {
			w.Neg(w)
		}
		out[i] = w.Int64()
	}
	return out
}

func amounts(parts []Money) []int64 {
	out := make([]int64, len(parts))
	for i, p := range parts {
		out[i] = p.Amount()
	}
	return out
}

func equal(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// typeCheck type-checks the package source files that are not tests, with go/types.
func typeCheck(t *testing.T) *types.Package {
	t.Helper()
	fset := token.NewFileSet()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	config := types.Config{Importer: importer.ForCompiler(fset, "source", nil)}
	pkg, err := config.Check("stego.dev/runtime/money", fset, files, nil)
	if err != nil {
		t.Fatalf("type-check the package: %v", err)
	}
	return pkg
}

// iso4217 is ISO 4217 list one (2025) with the minor unit of each code that has one, and ANG and BGN, which the list
// keeps while they are replaced. The codes whose minor unit is N.A. are not in it.
var iso4217 = map[string]int{
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
