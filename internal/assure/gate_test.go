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
			"ST-FIX-UC02":  {},
		}}
		held := Measurement{Obligations: map[string]Standing{
			"ST-FIX-INV01": all,
			"ST-FIX-INV02": all,
			"ST-FIX-INV03": {Designed: true, Built: true},
			"ST-FIX-SP01":  all,
			"ST-FIX-UC01":  {},
			"ST-FIX-UC02":  {},
			"ST-FIX-INV04": {},
			"ST-FIX-SP02":  {Designed: true},
		}}
		if reasons, followed := holdsRatchet(base, held, nil); len(reasons) != 0 || len(followed) != 0 {
			t.Fatalf("a change that keeps or gains every share and adds bare obligations is refused: %v %v", reasons, followed)
		}
		fell := Measurement{Obligations: map[string]Standing{
			"ST-FIX-SP01":  {Designed: true},
			"ST-FIX-INV05": {Designed: true},
			"ST-FIX-INV02": {Designed: true, Proved: true},
			"ST-FIX-INV03": {},
			"ST-FIX-UC01":  {},
			"ST-FIX-INV04": {},
		}}
		want := []string{
			"ST-FIX-INV01 is absent at the change (was designed, built, proved at the base)",
			"ST-FIX-INV02 fell from built",
			"ST-FIX-INV03 fell from designed",
			"ST-FIX-SP01 fell from built", "ST-FIX-SP01 fell from proved",
		}
		if reasons, followed := holdsRatchet(base, fell, nil); !reflect.DeepEqual(reasons, want) || len(followed) != 0 {
			t.Fatalf("got %q and %q, want %q", reasons, followed, want)
		}

		// Retirements at the change name the absent obligations. INV01 moves to INV06, which has each share it
		// had; INV02 moves to INV07, which is not proved, and to INV08, which is not built; INV03 goes with no
		// successor; SP01 is absent and no retirement names it; a retirement of UC09, which the base does not
		// have, is not followed.
		retirements := []Retirement{
			{ID: "ST-FIX-RT01", Retired: "ST-FIX-INV01", Successors: []string{"ST-FIX-INV06"}},
			{ID: "ST-FIX-RT02", Retired: "ST-FIX-INV02", Successors: []string{"ST-FIX-INV07", "ST-FIX-INV08"}},
			{ID: "ST-FIX-RT03", Retired: "ST-FIX-INV03"},
			{ID: "ST-FIX-RT04", Retired: "ST-FIX-UC09", Successors: []string{"ST-FIX-UC01"}},
		}
		moved := Measurement{Obligations: map[string]Standing{
			"ST-FIX-INV06": all,
			"ST-FIX-INV07": {Designed: true, Built: true},
			"ST-FIX-INV08": {Designed: true},
			"ST-FIX-UC01":  {},
			"ST-FIX-UC02":  {},
		}}
		want = []string{
			"ST-FIX-INV02 is retired by ST-FIX-RT02, but its successor ST-FIX-INV08 is not built",
			"ST-FIX-SP01 is absent at the change (was designed, built, proved at the base)",
		}
		wantFollowed := []string{
			"retirement ST-FIX-RT01: ST-FIX-INV01 succeeded by ST-FIX-INV06",
			"retirement ST-FIX-RT03: ST-FIX-INV03 has no successor",
		}
		reasons, followed := holdsRatchet(base, moved, retirements)
		if !reflect.DeepEqual(reasons, want) || !reflect.DeepEqual(followed, wantFollowed) {
			t.Fatalf("got %q and %q, want %q and %q", reasons, followed, want, wantFollowed)
		}
		// A pattern rule is an obligation in each domain that follows its pattern. A retirement of the rule names
		// it in each domain, compares its successor in the same domain, and is reported once.
		patternBase := Measurement{Obligations: map[string]Standing{
			"ST-FIX-PAT-R01@ST-FIX": all, "ST-FIX-PAT-R01@ST-SHOP": {Designed: true, Built: true},
		}}
		renamed := Measurement{Obligations: map[string]Standing{
			"ST-FIX-PAT-R02@ST-FIX": all, "ST-FIX-PAT-R02@ST-SHOP": {Designed: true, Built: true},
		}}
		rename := []Retirement{{ID: "ST-FIX-RT05", Retired: "ST-FIX-PAT-R01", Successors: []string{"ST-FIX-PAT-R02"}}}
		reasons, followed = holdsRatchet(patternBase, renamed, rename)
		if len(reasons) != 0 || !reflect.DeepEqual(followed, []string{"retirement ST-FIX-RT05: ST-FIX-PAT-R01 succeeded by ST-FIX-PAT-R02"}) {
			t.Fatalf("a renamed pattern rule gives %q and %q", reasons, followed)
		}
		unproved := Measurement{Obligations: map[string]Standing{
			"ST-FIX-PAT-R02@ST-FIX": {Designed: true, Built: true}, "ST-FIX-PAT-R02@ST-SHOP": {Designed: true, Built: true},
		}}
		reasons, followed = holdsRatchet(patternBase, unproved, rename)
		if !reflect.DeepEqual(reasons, []string{"ST-FIX-PAT-R01@ST-FIX is retired by ST-FIX-RT05, but its successor ST-FIX-PAT-R02@ST-FIX is not proved"}) ||
			!reflect.DeepEqual(followed, []string{"retirement ST-FIX-RT05: ST-FIX-PAT-R01 succeeded by ST-FIX-PAT-R02"}) {
			t.Fatalf("a renamed pattern rule not proved in one domain gives %q and %q", reasons, followed)
		}
		if sorted := sortReasons([]string{want[1], want[0], "ST-FIX-INV02 fell from proved", "ST-FIX-INV01-T failed"}); !reflect.DeepEqual(sorted,
			[]string{want[0], "ST-FIX-INV02 fell from proved", want[1], "ST-FIX-INV01-T failed"}) {
			t.Fatalf("a retirement's reason is not in the fixed order among the obligations: %q", sorted)
		}
	})
}
