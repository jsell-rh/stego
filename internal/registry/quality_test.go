package registry

import (
	"strings"
	"testing"
)

func TestQualityEvidenceKind(t *testing.T) {
	t.Run("ST-VO47-C01-T A quality evidence kind is one of the five kinds of the bar, and no other value", func(t *testing.T) {
		for _, value := range []string{"security-review", "benchmark", "design-review", "reference-comparison", "conformance"} {
			kind, err := ParseEvidenceKind(value)
			if err != nil || string(kind) != value {
				t.Fatalf("%q was refused: %v", value, err)
			}
		}
		for _, value := range []string{"review", "Benchmark", "performance", "conformance-pass", ""} {
			_, err := ParseEvidenceKind(value)
			if err == nil {
				t.Fatalf("%q was accepted", value)
			}
			for _, word := range []string{`"` + value + `"`, "security-review", "benchmark", "design-review", "reference-comparison", "conformance"} {
				if !strings.Contains(err.Error(), word) {
					t.Fatalf("the refusal of %q does not name %s: %v", value, word, err)
				}
			}
		}
	})
}

func TestContractSetBestOfBreed(t *testing.T) {
	t.Run("ST-SUP-SP07-T A component with an undecided reference promise is not best of breed", func(t *testing.T) {
		comparison := &QualityEvidence{
			Component: "exp-a", Kind: EvidenceReferenceComparison, ContractSetDigest: "set-1", References: []string{"ref-1 2.0"},
			Promises: []ComparedPromise{
				{Reference: "ref-1 2.0", Promise: "p1", Verdict: "adopted", Contract: "ST-SUP-RC01-OC01"},
				{Reference: "ref-1 2.0", Promise: "p2", Verdict: "declined", Reason: "not in scope"},
			},
		}
		if err := bestOfBreed(comparison, "set-1"); err != nil {
			t.Fatalf("a comparison that decides every promise does not hold: %v", err)
		}
		comparison.Promises = append(comparison.Promises, ComparedPromise{Reference: "ref-1 2.0", Promise: "p3"})
		err := bestOfBreed(comparison, "set-1")
		if err == nil || !strings.Contains(err.Error(), `"p3"`) || strings.Contains(err.Error(), `"p1"`) || strings.Contains(err.Error(), `"p2"`) {
			t.Fatalf("the undecided promise p3 alone is not named: %v", err)
		}
	})
}
