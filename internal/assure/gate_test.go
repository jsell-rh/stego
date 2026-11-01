package assure

import (
	"reflect"
	"strings"
	"testing"
)

// gateSpec is a spec at a commit with four automated test records and one that is not automated yet.
func gateSpec() Spec {
	return Spec{Tests: map[string]TestRecord{
		"ST-FIX-INV01-T":      {ID: "ST-FIX-INV01-T", Automated: true},
		"ST-FIX-INV02-T":      {ID: "ST-FIX-INV02-T", Automated: true},
		"ST-FIX-SP01-T":       {ID: "ST-FIX-SP01-T", Automated: true},
		"ST-FIX-UC01-R01-T01": {ID: "ST-FIX-UC01-R01-T01", Automated: true},
		"ST-FIX-SP02-T":       {ID: "ST-FIX-SP02-T"},
	}}
}

// verifiedRun is a verified proof run with the outcomes given as "ID result" or "ID failed: did not run".
func verifiedRun(outcomes ...string) *VerifiedProofRun {
	proof := &VerifiedProofRun{Grade: ReleaseGrade}
	for _, entry := range outcomes {
		id, rest, _ := strings.Cut(entry, " ")
		result, detail, _ := strings.Cut(rest, ": ")
		proof.Run.Outcomes = append(proof.Run.Outcomes, Outcome{TestRecord: id, Result: Result(result), Detail: detail})
	}
	return proof
}

func TestGate(t *testing.T) {
	t.Run("ST-VO57-C01-T A gate kind is exactly change or release", func(t *testing.T) {
		for _, text := range []string{"change", "release"} {
			kind, err := ParseGateKind(text)
			if err != nil || string(kind) != text {
				t.Fatalf("%q reads as %q, %v", text, kind, err)
			}
		}
		for _, text := range []string{"", "Change", "RELEASE", " change", "release\n", "merge", "change,release", "ratchet"} {
			if kind, err := ParseGateKind(text); err == nil {
				t.Fatalf("%q is accepted as the gate kind %q", text, kind)
			}
		}
	})

	t.Run("ST-ASR-SP01-T A proof run passed only when it is verified and every automated test record passed", func(t *testing.T) {
		spec := gateSpec()
		passed := verifiedRun("ST-FIX-INV01-T passed", "ST-FIX-INV02-T passed", "ST-FIX-SP01-T passed", "ST-FIX-UC01-R01-T01 passed", "ST-FIX-SP02-T failed")
		if reasons := proofRunPassed(passed, spec, "no proof run"); len(reasons) != 0 {
			t.Fatalf("a proof run in which every automated test record passed did not pass: %v", reasons)
		}
		mixed := verifiedRun("ST-FIX-UC01-R01-T01 skipped", "ST-FIX-INV02-T failed: did not run", "ST-FIX-INV01-T failed", "ST-FIX-SP02-T failed")
		want := []string{"ST-FIX-INV01-T failed", "ST-FIX-INV02-T failed: did not run", "ST-FIX-SP01-T has no outcome in the proof run", "ST-FIX-UC01-R01-T01 skipped"}
		if reasons := proofRunPassed(mixed, spec, "no proof run"); !reflect.DeepEqual(reasons, want) {
			t.Fatalf("got %q, want %q", reasons, want)
		}
		if reasons := proofRunPassed(nil, spec, "no verified proof run at the commit"); !reflect.DeepEqual(reasons, []string{"no verified proof run at the commit"}) {
			t.Fatalf("with no verified proof run: %q", reasons)
		}
		if reasons := proofRunPassed(nil, Spec{}, "no verified proof run at the commit"); len(reasons) != 1 {
			t.Fatalf("with no verified proof run and no automated test record: %q", reasons)
		}
	})

	t.Run("ST-ASR-SP03-T A change holds the ratchet unless an obligation present at the base lost a share, and new obligations may arrive with none", func(t *testing.T) {
		all := Standing{Designed: true, Built: true, Proved: true}
		base := Measurement{Obligations: map[string]Standing{
			"ST-FIX-INV01": all,
			"ST-FIX-INV02": {Designed: true, Built: true},
			"ST-FIX-INV03": {Designed: true},
			"ST-FIX-SP01":  all,
			"ST-FIX-UC01":  {},
		}}
		held := Measurement{Obligations: map[string]Standing{
			"ST-FIX-INV01": all,
			"ST-FIX-INV02": all,
			"ST-FIX-INV03": {Designed: true, Built: true},
			"ST-FIX-SP01":  all,
			"ST-FIX-UC01":  {},
			"ST-FIX-INV04": {},
			"ST-FIX-SP02":  {Designed: true},
		}}
		if reasons := holdsRatchet(base, held); len(reasons) != 0 {
			t.Fatalf("a change that keeps or gains every share and adds bare obligations is refused: %v", reasons)
		}
		fell := Measurement{Obligations: map[string]Standing{
			"ST-FIX-SP01":  {Designed: true},
			"ST-FIX-INV02": {Designed: true, Proved: true},
			"ST-FIX-INV03": {},
			"ST-FIX-UC01":  {},
			"ST-FIX-INV04": {},
		}}
		want := []string{
			"ST-FIX-INV01 fell from designed", "ST-FIX-INV01 fell from built", "ST-FIX-INV01 fell from proved",
			"ST-FIX-INV02 fell from built",
			"ST-FIX-INV03 fell from designed",
			"ST-FIX-SP01 fell from built", "ST-FIX-SP01 fell from proved",
		}
		if reasons := holdsRatchet(base, fell); !reflect.DeepEqual(reasons, want) {
			t.Fatalf("got %q, want %q", reasons, want)
		}
	})
}
