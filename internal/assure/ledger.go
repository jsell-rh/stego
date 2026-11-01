package assure

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// NoResult is the result of a test record at a commit where no verified proof run gives it an outcome: the
// ledger shows it as none (ST-ASR-RM01, Result).
const NoResult Result = ""

// DefaultLedgerDepth is how many commits of the first-parent history BuildLedger walks, from the commit itself,
// when the request names no depth.
const DefaultLedgerDepth = 200

// LedgerRow is one row of the proof ledger (ST-ASR-RM01): a test record of the spec at the commit, its state
// there, its result in the verified proof run at the commit (NoResult when none gives it an outcome), and the
// newest commit at which a verified proof run passed it (empty when none did within the commits walked).
type LedgerRow struct {
	TestRecord   string
	State        string
	Result       Result
	LastPassedAt string
}

// LedgerRefusal is why VerifyAttestation refused each attestation it found for one commit the ledger walked, in
// the order it gives them.
type LedgerRefusal struct {
	Commit  string
	Reasons []string
}

// Ledger is the table of the proof ledger (ST-ASR-RM01-TB), which the run builds in memory and keeps only while
// it runs (ST-STORE05). It holds a row for each test record of the spec at the commit, in ID order; the proof run
// verified at the commit, or none; how many commits of the first-parent history were walked and the most that
// could be; each attestation refused at a commit walked, in walk order; and each change-grade attestation at a
// commit walked, in walk order, which gives no result.
type Ledger struct {
	Commit      string
	Rows        []LedgerRow
	Proof       *VerifiedProofRun
	Walked      int
	Depth       int
	Refused     []LedgerRefusal
	ChangeGrade []LedgerChangeGrade
}

// LedgerChangeGrade is a change-grade attestation at a commit the ledger walked: the trusted workflow signed it
// at a pull request's reference, so it counts only in that change's own gate and gives the ledger no result.
type LedgerChangeGrade struct {
	Commit   string
	Ref      string
	LogEntry string
}

// LedgerRequest is what the proof ledger is built from: the repository that holds the commit, the commit, the
// provider's trusted signer, and how many commits of history to walk at most (DefaultLedgerDepth when zero). The
// ledger is for anyone, with no gate's context, so it takes no change reference: only a release-grade proof run,
// signed at main, gives results. Change-grade evidence counts only in its own change's gate (ST-ASR-SP02).
type LedgerRequest struct {
	Root    string
	Commit  string
	Trusted TrustedSigner
	Depth   int
}

// BuildLedger is the projection of ST-ASR-RM01, Proof ledger. It reads the test records and their states from the
// spec at the commit in the repository's git objects, never from the working tree, and reads no evidence file.
// Each result and each pass comes only from a proof run attestation that VerifyAttestation (ST-ASR-DS03) fetched
// by its commit and accepted at release grade, signed at main (ST-ASR-SP02); the ledger trusts no change reference,
// so a change-grade attestation is listed apart and gives no result. It walks the commit's first-parent history from the commit itself,
// fetching each commit's attestations once, and takes for each test record the first commit at which a verified
// proof run passed it. It stops when every automated test record has passed somewhere, at the root commit, or
// after Depth commits. A commit with no attestation is passed over; a refused attestation is listed with its
// reasons and gives no result. It returns an error, and no ledger, when it cannot read the spec at the commit or
// its history, or when the provider's store cannot answer.
func BuildLedger(request LedgerRequest, store AttestationStore, verifier BundleVerifier) (Ledger, error) {
	if !fullCommit(request.Commit) {
		return Ledger{}, fmt.Errorf("commit %q is not a full 40-character commit ID", request.Commit)
	}
	depth := request.Depth
	if depth == 0 {
		depth = DefaultLedgerDepth
	}
	if depth < 1 {
		return Ledger{}, fmt.Errorf("the depth %d is not a number of commits", depth)
	}
	spec, err := ReadCommittedSpec(request.Root, request.Commit)
	if err != nil {
		return Ledger{}, err
	}
	trust := Trust{Signer: request.Trusted}
	history, err := firstParentHistory(request.Root, request.Commit, depth)
	if err != nil {
		return Ledger{}, err
	}
	ledger := Ledger{Commit: request.Commit, Depth: depth}
	index := make(map[string]int, len(spec.Tests))
	for _, id := range spec.ids() {
		index[id] = len(ledger.Rows)
		ledger.Rows = append(ledger.Rows, LedgerRow{TestRecord: id, State: spec.Tests[id].State})
	}
	waiting := make(map[string]bool)
	for _, id := range spec.automated() {
		waiting[id] = true
	}
	for i, commit := range history {
		verification, err := VerifyAttestation(commit, store, verifier, trust)
		if err != nil {
			return Ledger{}, err
		}
		ledger.Walked++
		elsewhere := make(map[string]bool, len(verification.ChangeGrade))
		for _, change := range verification.ChangeGrade {
			ledger.ChangeGrade = append(ledger.ChangeGrade, LedgerChangeGrade{Commit: commit, Ref: change.Ref, LogEntry: change.LogEntry})
			elsewhere[change.Reason] = true
		}
		var reasons []string
		for _, reason := range verification.Refused {
			if !elsewhere[reason] {
				reasons = append(reasons, reason)
			}
		}
		if len(reasons) > 0 {
			ledger.Refused = append(ledger.Refused, LedgerRefusal{Commit: commit, Reasons: reasons})
		}
		if verification.Proof == nil {
			continue
		}
		if i == 0 {
			ledger.Proof = verification.Proof
		}
		for _, outcome := range verification.Proof.Run.Outcomes {
			row, ok := index[outcome.TestRecord]
			if !ok {
				continue
			}
			if i == 0 {
				ledger.Rows[row].Result = outcome.Result
			}
			if outcome.Result.Passes() && ledger.Rows[row].LastPassedAt == "" {
				ledger.Rows[row].LastPassedAt = commit
				delete(waiting, outcome.TestRecord)
			}
		}
		if len(waiting) == 0 {
			break
		}
	}
	return ledger, nil
}

// NotPassedAt is the handler of ST-ASR-QRY01, Find the proofs that did not pass at a commit. It reads only the
// ledger the reader holds, whose results come from verified attestations alone (ST-ASR-SP02). It lists, in test
// record ID order, every automated test record that has no passed outcome in the verified proof run at the
// commit, with its result there or none, and every test record in another state.
func NotPassedAt(reader LedgerReader) []LedgerRow {
	var rows []LedgerRow
	for _, row := range reader.Ledger().Rows {
		if row.State != automatedState || !row.Result.Passes() {
			rows = append(rows, row)
		}
	}
	return rows
}

// automatedState is the state of a test record that a file runs, as the spec gives it.
const automatedState = "automated"

// ids lists the IDs of every test record of the spec, in order.
func (s Spec) ids() []string {
	ids := make([]string, 0, len(s.Tests))
	for id := range s.Tests {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// firstParentHistory lists the commit and the commits before it along their first parents, newest first, at
// most depth of them, from the git objects of the repository at root.
func firstParentHistory(root, commit string, depth int) ([]string, error) {
	out, err := git(root, "rev-list", "--first-parent", fmt.Sprintf("--max-count=%d", depth), commit, "--")
	if err != nil {
		return nil, err
	}
	history := strings.Fields(string(out))
	if len(history) == 0 || history[0] != commit {
		return nil, errors.New("git gives no history for the commit " + commit)
	}
	return history, nil
}
