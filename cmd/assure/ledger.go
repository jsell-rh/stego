package main

import (
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/jsell-rh/stego/internal/assure"
)

// ledger is assure ledger (ST-ASR-CLI04). It builds the proof ledger of the commit from verified attestations and
// the spec in the repository's git objects, prints it, and asks NotPassedAt for the proofs that did not pass there.
// Only a release-grade proof run, signed at main, gives results; a change-grade one counts only in its own gate.
// It exits 0 when a verified proof run at the commit passed every automated test record, 1 when it did not or
// there is none, and 2 when it is called wrongly or cannot build the ledger, such as when the commit has no spec or
// the provider's store cannot answer.
func ledger(args []string, stdout, stderr io.Writer, getenv func(string) string, open provider) int {
	flags := flag.NewFlagSet("assure ledger", flag.ContinueOnError)
	flags.SetOutput(stderr)
	commit := flags.String("commit", "", "the full commit to show the proof ledger of")
	depth := flags.Int("depth", assure.DefaultLedgerDepth, "how many commits of first-parent history to walk, from the commit itself")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 || *commit == "" || *depth < 1 {
		fmt.Fprintln(stderr, "assure ledger: --commit is required, --depth is a number of commits from 1, and nothing else but --depth")
		return 2
	}
	built, err := buildLedger(*commit, *depth, getenv, open)
	if err != nil {
		fmt.Fprintf(stderr, "assure ledger: no ledger: %v\n", err)
		return 2
	}
	reader := assure.NewLedgerReader(built)
	notPassed := assure.NotPassedAt(reader)
	if _, err := io.WriteString(stdout, ledgerText(reader.Ledger(), notPassed)); err != nil {
		fmt.Fprintf(stderr, "assure ledger: %v\n", err)
		return 2
	}
	if built.Proof == nil {
		return 1
	}
	for _, row := range notPassed {
		if row.State == "automated" {
			return 1
		}
	}
	return 0
}

// buildLedger builds the proof ledger of the commit in the repository that holds the working folder, with the
// trusted signer from the environment. It reads no change reference: only a release-grade proof run gives results.
func buildLedger(commit string, depth int, getenv func(string) string, open provider) (assure.Ledger, error) {
	job, err := assure.ReadGitHubGate(getenv)
	if err != nil {
		return assure.Ledger{}, err
	}
	root, err := assure.RepositoryRoot(".")
	if err != nil {
		return assure.Ledger{}, err
	}
	store, verifier, done, err := open(getenv)
	if err != nil {
		return assure.Ledger{}, err
	}
	defer done()
	return assure.BuildLedger(assure.LedgerRequest{
		Root:    root,
		Commit:  commit,
		Trusted: job.Trusted,
		Depth:   depth,
	}, store, verifier)
}

// ledgerText is the proof ledger as assure ledger prints it: a heading line; one line per test record in ID order
// with its state, its result at the commit (passed, failed, skipped or none) and the last commit it passed at;
// one line for the release-grade proof run at the commit, or why there is none; a line for each change-grade
// attestation at a commit walked, which counts only in its own gate; a line for each attestation refused at a commit
// walked, with the reason; and the test records NotPassedAt gives. The same ledger always gives the same text.
func ledgerText(ledger assure.Ledger, notPassed []assure.LedgerRow) string {
	var text strings.Builder
	fmt.Fprintf(&text, "proof ledger at %s: %s, %s walked of at most %d\n",
		ledger.Commit, count(len(ledger.Rows), "test record"), count(ledger.Walked, "commit"), ledger.Depth)
	for _, row := range ledger.Rows {
		result := string(row.Result)
		if row.Result == assure.NoResult {
			result = "none"
		}
		last := "last passed at " + row.LastPassedAt
		if row.LastPassedAt == "" {
			last = "last passed not within " + count(ledger.Walked, "commit")
		}
		fmt.Fprintf(&text, "%s (%s): %s, %s\n", row.TestRecord, row.State, result, last)
	}
	switch proof := ledger.Proof; {
	case proof != nil:
		fmt.Fprintf(&text, "proof run at %s: %s grade, signed by %s (issuer %s), %s\n",
			ledger.Commit, proof.Grade, proof.Attestation.Signer.Subject, proof.Attestation.Signer.Issuer, proof.Attestation.LogEntry)
	case attestedAt(ledger, ledger.Commit):
		fmt.Fprintf(&text, "proof run at %s: none verified at release grade\n", ledger.Commit)
	default:
		fmt.Fprintf(&text, "proof run at %s: none, as the provider keeps no proof run attestation for it\n", ledger.Commit)
	}
	for _, change := range ledger.ChangeGrade {
		fmt.Fprintf(&text, "proof run at %s: change grade at %s (%s): counts only in its own gate\n", change.Commit, change.Ref, change.LogEntry)
	}
	for _, refusal := range ledger.Refused {
		for _, reason := range refusal.Reasons {
			fmt.Fprintf(&text, "refused at %s: %s\n", refusal.Commit, strings.Join(strings.Fields(reason), " "))
		}
	}
	if len(notPassed) == 0 {
		text.WriteString("not passed: none\n")
		return text.String()
	}
	ids := make([]string, 0, len(notPassed))
	for _, row := range notPassed {
		ids = append(ids, row.TestRecord)
	}
	fmt.Fprintf(&text, "not passed (%d): %s\n", len(ids), strings.Join(ids, ", "))
	return text.String()
}

// count is the number n and the noun, with an s after the noun when n is not 1.
func count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// attestedAt tells if the ledger holds a refused or a change-grade attestation at the commit.
func attestedAt(ledger assure.Ledger, commit string) bool {
	for _, refusal := range ledger.Refused {
		if refusal.Commit == commit {
			return true
		}
	}
	for _, change := range ledger.ChangeGrade {
		if change.Commit == commit {
			return true
		}
	}
	return false
}
