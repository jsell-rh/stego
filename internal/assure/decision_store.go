package assure

import (
	"fmt"
	"io"
	"os"
)

// DecisionStore is the repository of gate decisions (ST-ASR-REPO02). Keep writes a decision to the CI job's
// output. It writes nothing into the product's repository. Handing a passed release decision to the provider's
// attestation action, as the decision.json table (ST-ASR-EN03-TB), belongs to Gate a release (ST-ASR-UC03).
type DecisionStore interface {
	Keep(decision Decision) error
}

// JobOutput keeps a decision in the CI job's output: its text on Out, the job's log, and, when Summary names
// the job's step summary file (GITHUB_STEP_SUMMARY), at the end of that file too, which must be outside the
// product's repository at Repository.
type JobOutput struct {
	Repository string
	Out        io.Writer
	Summary    string
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
