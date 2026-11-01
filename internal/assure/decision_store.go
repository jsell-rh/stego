package assure

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// DecisionStore is the repository of gate decisions (ST-ASR-REPO02). Keep writes a decision to the CI job's
// output. HandOver writes a passed release decision as its predicate, the decision.json table (ST-ASR-EN03-TB,
// DecisionFile), where the provider's attestation action reads it, signs it with the release commit as its subject
// and keeps it, and returns the predicate's digest; it refuses any other decision. Neither writes into the
// product's repository.
type DecisionStore interface {
	Keep(decision Decision) error
	HandOver(decision Decision) (string, error)
}

// JobOutput keeps a decision in the CI job's output: its text on Out, the job's log, and, when Summary names
// the job's step summary file (GITHUB_STEP_SUMMARY), at the end of that file too, which must be outside the
// product's repository at Repository. It hands a passed release decision over as the file Predicate, outside the
// repository, which the job uploads for the attestation job.
type JobOutput struct {
	Repository string
	Out        io.Writer
	Summary    string
	Predicate  string
}

// Check tells, before a gate decides, whether the job output can keep a decision and hand one over: the step
// summary, when Summary names one, and the predicate file, when Predicate names one, must be outside the product's
// repository, in a folder that exists, and no predicate file may be there yet. An earlier predicate at that path is
// refused, not removed, so a refused release never leaves a predicate that the job could upload as its own.
func (j JobOutput) Check() error {
	if j.Summary != "" {
		if err := outsideTheRepository(j.Repository, j.Summary); err != nil {
			return err
		}
	}
	if j.Predicate == "" {
		return nil
	}
	if err := outsideTheRepository(j.Repository, j.Predicate); err != nil {
		return err
	}
	if _, err := os.Lstat(j.Predicate); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("a file is already at the decision predicate's path %s (%v); remove it first, so only this run's decision can be attested", j.Predicate, err)
	}
	return nil
}

// HandOver writes the passed release decision's predicate to the file Predicate, and returns its digest. It
// refuses a decision that is not a passed release decision with the attestation it is to get, and writes nothing.
func (j JobOutput) HandOver(decision Decision) (string, error) {
	if decision.Kind != ReleaseGate || !decision.Passed || decision.Attestation == nil {
		return "", fmt.Errorf("only a passed release decision is handed to the attestation action, not the %s decision on %s (passed=%t)",
			decision.Kind, decision.Commit, decision.Passed)
	}
	if j.Predicate == "" {
		return "", errors.New("no predicate file is named for the release decision")
	}
	data, digest, err := encodeDecision(decision)
	if err != nil {
		return "", err
	}
	if err := writeOutside(j.Repository, j.Predicate, data); err != nil {
		return "", err
	}
	return digest, nil
}

// Keep writes the decision's text.
func (j JobOutput) Keep(decision Decision) error {
	text := decision.Text()
	if j.Summary != "" {
		if err := outsideTheRepository(j.Repository, j.Summary); err != nil {
			return err
		}
		file, err := os.OpenFile(j.Summary, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(file, "```text\n%s```\n", text)
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
	}
	_, err := io.WriteString(j.Out, text)
	return err
}

// KeepNoDecision writes one line to the job's step summary, when Summary names one, that says the gate reached no
// decision and why, so a gate that exits without a decision never leaves the summary empty or showing a decision
// that the job does not stand by. The gate is change or release; the reason is given on one line. When Repository
// names the product's repository, the summary must be outside it, as for Keep.
func (j JobOutput) KeepNoDecision(gate GateKind, why string) error {
	if j.Summary == "" {
		return nil
	}
	if j.Repository != "" {
		if err := outsideTheRepository(j.Repository, j.Summary); err != nil {
			return err
		}
	}
	file, err := os.OpenFile(j.Summary, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(file, "%s gate: no decision was reached: %s\n", gate, strings.Join(strings.Fields(why), " "))
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}
