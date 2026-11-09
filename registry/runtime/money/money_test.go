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
