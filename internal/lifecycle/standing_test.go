package lifecycle

import (
	"testing"
	"time"
)

func date(t *testing.T, text string) time.Time {
	t.Helper()
	day, err := ParseDate(text)
	if err != nil {
		t.Fatal(err)
	}
	return day
}

func digest(letter string) string {
	result := ""
	for len(result) < 64 {
		result += letter
	}
	return result
}

func superseded(t *testing.T, failsAfter string) Lifecycle {
	return Lifecycle{
		Subject:      Subject{Kind: ComponentPackage, Name: "a", Digest: digest("1")},
		Supersession: &Supersession{Successor: Subject{Kind: ComponentPackage, Name: "a", Digest: digest("2")}, FailsAfter: date(t, failsAfter), AdvisoryID: "STEGO-2026-0001"},
	}
}

func withdrawn(t *testing.T, failsAfter string) Lifecycle {
	return Lifecycle{
		Subject:    Subject{Kind: ComponentPackage, Name: "a", Digest: digest("1")},
		Withdrawal: &Withdrawal{FailsAfter: date(t, failsAfter), Reason: "leaks a token in a log", AdvisoryID: "STEGO-2026-0002"},
	}
}

func noRecord() Lifecycle {
	return Lifecycle{Subject: Subject{Kind: ComponentPackage, Name: "a", Digest: digest("1")}}
}

func TestDigestIsSuperseded(t *testing.T) {
	t.Run("ST-DIS-SP02-T A digest is superseded only when its lifecycle holds a supersession", func(t *testing.T) {
		for _, c := range []struct {
			name string
			l    Lifecycle
			want bool
		}{
			{"a supersession whose fail date is ahead", superseded(t, "2099-01-01"), true},
			{"a supersession whose fail date is past", superseded(t, "2000-01-01"), true},
			{"a withdrawal only", withdrawn(t, "2099-01-01"), false},
			{"no record", noRecord(), false},
		} {
			if got := IsSuperseded(c.l); got != c.want {
				t.Errorf("%s: superseded is %v, want %v", c.name, got, c.want)
			}
		}
	})
}

func TestDigestIsPastItsFailDate(t *testing.T) {
	t.Run("ST-DIS-SP03-T A digest is past its fail date only after the date of its supersession or withdrawal", func(t *testing.T) {
		day := date(t, "2026-06-01")
		for _, c := range []struct {
			name string
			l    Lifecycle
			want bool
		}{
			{"a supersession that fails after 2026-05-31", superseded(t, "2026-05-31"), true},
			{"a supersession that fails after 2026-06-01", superseded(t, "2026-06-01"), false},
			{"a supersession that fails after 2026-06-02", superseded(t, "2026-06-02"), false},
			{"a withdrawal that fails after 2026-05-31", withdrawn(t, "2026-05-31"), true},
			{"a withdrawal that fails after 2026-06-02", withdrawn(t, "2026-06-02"), false},
			{"no record", noRecord(), false},
		} {
			if got := IsPastFailDate(c.l, day); got != c.want {
				t.Errorf("%s: past its fail date on 2026-06-01 is %v, want %v", c.name, got, c.want)
			}
		}
	})
}

func TestDigestIsWithdrawn(t *testing.T) {
	t.Run("ST-DIS-SP04-T A digest is withdrawn when its lifecycle holds a withdrawal, whatever its date", func(t *testing.T) {
		for _, c := range []struct {
			name string
			l    Lifecycle
			want bool
		}{
			{"a withdrawal whose fail date is ahead", withdrawn(t, "2099-01-01"), true},
			{"a withdrawal whose fail date is past", withdrawn(t, "2000-01-01"), true},
			{"a supersession only", superseded(t, "2099-01-01"), false},
			{"no record", noRecord(), false},
		} {
			if got := IsWithdrawn(c.l); got != c.want {
				t.Errorf("%s: withdrawn is %v, want %v", c.name, got, c.want)
			}
		}
	})
}
