package assure

import (
	"reflect"
	"strings"
	"testing"
)

func TestRelease(t *testing.T) {
	t.Run("ST-ASR-SP04-T A commit is fully proved only when every share is 100% and a release-grade proof run at it passed", func(t *testing.T) {
		commit := strings.Repeat("ab", 20)
		spec := gateSpec()
		all := Standing{Designed: true, Built: true, Proved: true}
		full := Measurement{Commit: commit, Obligations: map[string]Standing{"ST-FIX-INV01": all, "ST-FIX-SP01": all, "ST-FIX-UC01-R01": all}}
		passed := verifiedRun("ST-FIX-INV01-T passed", "ST-FIX-INV02-T passed", "ST-FIX-SP01-T passed", "ST-FIX-UC01-R01-T01 passed")
		passed.Attestation = Attestation{PredicateType: ProofRunPredicateType, Subject: commit, LogEntry: "rekor:7",
			Signer: Signer{Issuer: GitHubIssuer, Subject: "https://github.com/jsell-rh/stego/.github/workflows/proof-run.yml@" + MainRef}}
		why := noVerifiedProofRun + commit + ": the provider keeps no proof run attestation for it"
		if reasons := fullyProved(full, passed, spec, why); len(reasons) != 0 {
			t.Fatalf("a commit with every share and a passed release-grade proof run is not fully proved: %q", reasons)
		}

		short := Measurement{Commit: commit, Obligations: map[string]Standing{
			"ST-FIX-SP01":     {Designed: true},
			"ST-FIX-INV01":    all,
			"ST-FIX-UC01-R01": {Designed: true, Built: true},
			"ST-FIX-INV02":    {},
		}}
		failed := verifiedRun("ST-FIX-INV01-T passed", "ST-FIX-INV02-T failed", "ST-FIX-SP01-T passed")
		failed.Attestation = passed.Attestation
		want := []string{
			"ST-FIX-INV02 is not designed", "ST-FIX-INV02 is not built", "ST-FIX-INV02 is not proved",
			"ST-FIX-SP01 is not built", "ST-FIX-SP01 is not proved",
			"ST-FIX-UC01-R01 is not proved",
			"ST-FIX-INV02-T failed", "ST-FIX-UC01-R01-T01 has no outcome in the proof run",
		}
		if reasons := fullyProved(short, failed, spec, why); !reflect.DeepEqual(reasons, want) {
			t.Fatalf("got %q, want %q", reasons, want)
		}
		if reasons := sortReasons(want); !reflect.DeepEqual(reasons, want) {
			t.Fatalf("the reasons are not in the fixed order: %q", reasons)
		}

		if reasons := fullyProved(full, nil, spec, why); !reflect.DeepEqual(reasons, []string{why}) {
			t.Fatalf("with no verified proof run: %q", reasons)
		}
		change := *passed
		change.Grade = ChangeGrade
		change.Attestation.Signer.Subject = "https://github.com/jsell-rh/stego/.github/workflows/proof-run.yml@refs/pull/7/merge"
		want = []string{noVerifiedProofRun + commit + ": the proof run at rekor:7 is signed at refs/pull/7/merge, not at " + MainRef +
			", and a release takes only release-grade evidence"}
		if reasons := fullyProved(full, &change, spec, why); !reflect.DeepEqual(reasons, want) {
			t.Fatalf("with a change-grade proof run: got %q, want %q", reasons, want)
		}
		if reasons := fullyProved(Measurement{Commit: commit}, passed, Spec{}, why); len(reasons) != 0 {
			t.Fatalf("an empty spec with a passed proof run is not fully proved: %q", reasons)
		}
	})
}
