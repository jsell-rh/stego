package assure

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// AttestationStore is the repository of proof runs (ST-ASR-REPO01). It keeps a proof run only as an attestation
// at the CI provider, with the commit as its subject: HandOver hands the proof run's statement to the provider's
// attestation action, which signs and keeps it, and Fetch reads back what the provider keeps for a commit. The
// product's repository holds nothing (ST-ASR-A1-SR01).
//
// The store only moves bytes. VerifyAttestation (ST-ASR-DS03) checks each bundle that Fetch gives before the
// gates read it, so a store that gives a wrong or forged bundle gives no attestation.
type AttestationStore interface {
	// HandOver writes the proof run's predicate where the provider's attestation action reads it, and returns
	// the predicate's digest. It writes nothing in the product's repository.
	HandOver(run ProofRun) (string, error)
	// Fetch returns every signed bundle the provider keeps under SubjectDigest of the commit, as the provider
	// gives it, and none when the provider keeps none. An empty bundle stands for one the provider names but
	// does not give.
	Fetch(commit string) ([][]byte, error)
}

// ProofRunFile is the name of the file that holds a proof run's predicate, the proof-run.json table
// (ST-ASR-EN01-TB), outside the product's repository.
const ProofRunFile = "proof-run.json"

// proofRunDocument is the predicate of a proof run, version 1 of ProofRunPredicateType: one JSON object with the
// run's fields and its outcomes in test record ID order. The same proof run always gives the same bytes.
type proofRunDocument struct {
	Commit       string            `json:"commit"`
	Provider     string            `json:"provider"`
	RunReference string            `json:"runReference"`
	StartedAt    string            `json:"startedAt"`
	FinishedAt   string            `json:"finishedAt"`
	SpecDigest   string            `json:"specDigest"`
	Outcomes     []outcomeDocument `json:"outcomes"`
}

type outcomeDocument struct {
	TestRecord string   `json:"testRecord"`
	Result     Result   `json:"result"`
	File       string   `json:"file"`
	Duration   *float64 `json:"duration,omitempty"`
	Detail     string   `json:"detail,omitempty"`
}

// encodeProofRun writes the proof run's predicate, with a newline at its end.
func encodeProofRun(run ProofRun) ([]byte, error) {
	document := proofRunDocument{
		Commit:       run.Commit,
		Provider:     run.Provider,
		RunReference: run.RunReference,
		StartedAt:    run.StartedAt.UTC().Format(time.RFC3339Nano),
		FinishedAt:   run.FinishedAt.UTC().Format(time.RFC3339Nano),
		SpecDigest:   run.SpecDigest,
		Outcomes:     make([]outcomeDocument, 0, len(run.Outcomes)),
	}
	for _, outcome := range run.Outcomes {
		entry := outcomeDocument{TestRecord: outcome.TestRecord, Result: outcome.Result, File: outcome.File, Detail: outcome.Detail}
		if outcome.Timed {
			duration := outcome.Duration
			entry.Duration = &duration
		}
		document.Outcomes = append(document.Outcomes, entry)
	}
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// outsideTheRepository is the rule of ST-ASR-INV05: a proof run is never written as a file in the product's
// repository. It refuses a file inside the repository's folder, also through a symbolic link. The file's folder
// must exist.
func outsideTheRepository(repository, file string) error {
	refuse := func(format string, args ...any) error {
		return &Refusal{Rule: "outsideTheRepository", Reason: fmt.Sprintf(format, args...)}
	}
	root, err := filepath.EvalSymlinks(repository)
	if err != nil {
		return err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return err
	}
	absolute, err := filepath.Abs(file)
	if err != nil {
		return err
	}
	folder, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return err
	}
	target := filepath.Join(folder, filepath.Base(absolute))
	if resolved, err := filepath.EvalSymlinks(target); err == nil {
		target = resolved
	}
	relative, err := filepath.Rel(root, target)
	if err != nil {
		return err
	}
	if relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return refuse("%s is inside the repository %s; a proof run is kept only as the provider's attestation", file, repository)
	}
	return nil
}

// writeOutside writes data to the file, outside the repository, through a new file in the same folder that
// then takes the file's name, so a reader never sees half of it.
func writeOutside(repository, file string, data []byte) error {
	if err := outsideTheRepository(repository, file); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(file), "."+filepath.Base(file)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporary.Name(), file)
}
