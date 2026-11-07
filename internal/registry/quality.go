package registry

import (
	"fmt"
	"strings"
)

// EvidenceKind is which part of the ga quality bar one piece of evidence
// shows, spelled as stego registry evidence --kind writes it.
type EvidenceKind string

const (
	// EvidenceSecurityReview is a security review of the component's code.
	EvidenceSecurityReview EvidenceKind = "security-review"
	// EvidenceBenchmark is a benchmark of the component against a reference.
	EvidenceBenchmark EvidenceKind = "benchmark"
	// EvidenceDesignReview is a design review of the component.
	EvidenceDesignReview EvidenceKind = "design-review"
	// EvidenceReferenceComparison compares the component's contract set with
	// the promises of the best-in-field references.
	EvidenceReferenceComparison EvidenceKind = "reference-comparison"
	// EvidenceConformance is a pass of the conformance suite of a port the
	// component provides.
	EvidenceConformance EvidenceKind = "conformance"
)

var evidenceKinds = []EvidenceKind{EvidenceSecurityReview, EvidenceBenchmark, EvidenceDesignReview, EvidenceReferenceComparison, EvidenceConformance}

// ParseEvidenceKind gives the kind a value names, and refuses any value that
// is not exactly one of the five kinds, naming the value and the five kinds.
func ParseEvidenceKind(value string) (EvidenceKind, error) {
	for _, kind := range evidenceKinds {
		if string(kind) == value {
			return kind, nil
		}
	}
	names := make([]string, len(evidenceKinds))
	for i, kind := range evidenceKinds {
		names[i] = string(kind)
	}
	return "", fmt.Errorf("quality evidence kind %q is not valid; it must be one of %s", value, strings.Join(names, ", "))
}

// QualityEvidence is one piece of evidence that a component version meets
// the ga quality bar, made at the component's exact digest; a reference
// comparison is made at the digest of the component's contract set.
type QualityEvidence struct {
	Component         string            `yaml:"component"`
	ComponentDigest   string            `yaml:"component_digest"`
	Kind              EvidenceKind      `yaml:"kind"`
	Reviewer          string            `yaml:"reviewer"`
	Result            string            `yaml:"result"`
	Reference         string            `yaml:"reference"`
	RecordedAt        string            `yaml:"recorded_at"`
	ContractSetDigest string            `yaml:"contract_set_digest,omitempty"`
	References        []string          `yaml:"references,omitempty"`
	NoReferenceReason string            `yaml:"no_reference_reason,omitempty"`
	Promises          []ComparedPromise `yaml:"promises,omitempty"`
}

// ComparedPromise is one promise a reference makes, as a reference
// comparison judged it: adopted as a contract of the component, or declined
// with a reason.
type ComparedPromise struct {
	Reference string `yaml:"reference"`
	Promise   string `yaml:"promise"`
	Verdict   string `yaml:"verdict"`
	Contract  string `yaml:"contract,omitempty"`
	Reason    string `yaml:"reason,omitempty"`
}

// bestOfBreed evaluates ST-SUP-SP07: a component's contract set is best of
// breed when a reference comparison recorded at the set's current digest
// names at least one best-in-field reference, or states why none exists, and
// leaves no promise of a reference neither adopted as a contract nor declined
// with a reason. It gives nil when the set is best of breed, and otherwise an
// error that names the stale digests, the missing references or each
// undecided promise.
func bestOfBreed(comparison *QualityEvidence, contractSetDigest string) error {
	if comparison == nil || comparison.Kind != EvidenceReferenceComparison {
		return fmt.Errorf("the reference comparison is missing")
	}
	if comparison.ContractSetDigest != contractSetDigest {
		return fmt.Errorf("the reference comparison of %s is stale: it was made at the contract set digest %s, and the current digest is %s", comparison.Component, comparison.ContractSetDigest, contractSetDigest)
	}
	if len(comparison.References) == 0 && strings.TrimSpace(comparison.NoReferenceReason) == "" {
		return fmt.Errorf("the reference comparison of %s names no best-in-field reference and no reason why none exists", comparison.Component)
	}
	var undecided []string
	for _, promise := range comparison.Promises {
		adopted := promise.Verdict == "adopted" && strings.TrimSpace(promise.Contract) != ""
		declined := promise.Verdict == "declined" && strings.TrimSpace(promise.Reason) != ""
		if !adopted && !declined {
			undecided = append(undecided, fmt.Sprintf("%q of %s", promise.Promise, promise.Reference))
		}
	}
	if len(undecided) > 0 {
		return fmt.Errorf("the reference comparison of %s leaves promises undecided: %s", comparison.Component, strings.Join(undecided, ", "))
	}
	return nil
}
