package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/jsell-rh/stego/internal/assure"
)

// provider opens the CI provider's attestation store and the check of its signed bundles, from the job's
// environment.
type provider func(getenv func(string) string) (assure.AttestationStore, assure.BundleVerifier, func(), error)

// gate is assure gate change (ST-ASR-CLI02) and assure gate release (ST-ASR-CLI03). Each prints the decision with
// every reason, and exits 0 when the commit passes and 1 when it is refused. When it cannot decide, such as when
// Lathe finds an error in the spec at a commit it reads, it prints no decision, names why on stderr and in one line
// of the job's step summary, and exits 2.
func gate(args []string, stdout, stderr io.Writer, getenv func(string) string, open provider) int {
	if len(args) > 0 && args[0] == "change" {
		return gateChangeCommand(args[1:], stdout, stderr, getenv, open)
	}
	if len(args) > 0 && args[0] == "release" {
		return gateReleaseCommand(args[1:], stdout, stderr, getenv, open)
	}
	fmt.Fprintln(stderr, "assure gate: the gate is change or release: assure gate change --commit <sha> --base <sha>, or assure gate release --commit <sha> --predicate <out>")
	return 2
}

// gateChangeCommand is assure gate change (ST-ASR-CLI02). It gates the change's commit against the base head.
func gateChangeCommand(args []string, stdout, stderr io.Writer, getenv func(string) string, open provider) int {
	flags := flag.NewFlagSet("assure gate change", flag.ContinueOnError)
	flags.SetOutput(stderr)
	commit := flags.String("commit", "", "the full commit of the change")
	base := flags.String("base", "", "the full commit at the head of the branch the change merges into")
	lathe := flags.String("lathe", "", "the Lathe CLI (default node_modules/.bin/lathe of the repository)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 || *commit == "" || *base == "" {
		fmt.Fprintln(stderr, "assure gate change: --commit and --base are required, and nothing else but --lathe")
		return 2
	}
	decision, err := gateChange(*commit, *base, *lathe, stdout, getenv, open)
	if err != nil {
		fmt.Fprintf(stderr, "assure gate change: no decision: %v\n", err)
		keepNoDecision(assure.ChangeGate, err, stderr, getenv)
		return 2
	}
	if !decision.Passed {
		return 1
	}
	return 0
}

// gateReleaseCommand is assure gate release (ST-ASR-CLI03). It gates the release commit on its own, and writes
// the decision predicate for the provider's attestation action only when the release passes. After a refused
// decision it prints one notice line for the CI log, "::notice::release refused: N reasons", and exits 1.
func gateReleaseCommand(args []string, stdout, stderr io.Writer, getenv func(string) string, open provider) int {
	flags := flag.NewFlagSet("assure gate release", flag.ContinueOnError)
	flags.SetOutput(stderr)
	commit := flags.String("commit", "", "the full commit to release")
	predicate := flags.String("predicate", "", "the decision predicate file to write when the release passes, outside the repository")
	lathe := flags.String("lathe", "", "the Lathe CLI (default node_modules/.bin/lathe of the repository)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 || *commit == "" || *predicate == "" {
		fmt.Fprintln(stderr, "assure gate release: --commit and --predicate are required, and nothing else but --lathe")
		return 2
	}
	decision, err := gateRelease(*commit, *predicate, *lathe, stdout, getenv, open)
	if err != nil {
		fmt.Fprintf(stderr, "assure gate release: no decision: %v\n", err)
		keepNoDecision(assure.ReleaseGate, err, stderr, getenv)
		return 2
	}
	if !decision.Passed {
		// A refused release is a neutral outcome of the run (ST-DIS-UC11-R04): one notice line in the log says
		// so, after the decision, which the step summary holds.
		reasons := fmt.Sprintf("%d reasons", len(decision.Reasons))
		if len(decision.Reasons) == 1 {
			reasons = "1 reason"
		}
		fmt.Fprintf(stdout, "::notice::release refused: %s\n", reasons)
		return 1
	}
	return 0
}

// keepNoDecision writes one line to the job's step summary, when the job names one, that says the gate reached no
// decision and why. After a passed release decision whose predicate is not written, the summary already holds the
// decision; this line then says that the job does not stand by it. A summary inside the product's repository gets
// nothing, and a summary that cannot be written is named on stderr: the exit code stays 2 in either case.
func keepNoDecision(kind assure.GateKind, why error, stderr io.Writer, getenv func(string) string) {
	output := assure.JobOutput{Summary: getenv("GITHUB_STEP_SUMMARY")}
	if root, err := assure.RepositoryRoot("."); err == nil {
		output.Repository = root
	}
	if err := output.KeepNoDecision(kind, why.Error()); err != nil {
		fmt.Fprintf(stderr, "assure gate %s: the step summary does not say that no decision was reached: %v\n", kind, err)
	}
}

// gateChange gates the change in the repository that holds the working folder. It reads both commits from the
// repository's git objects, never from the working tree.
func gateChange(commit, base, lathe string, stdout io.Writer, getenv func(string) string, open provider) (assure.Decision, error) {
	job, err := assure.ReadGitHubGate(getenv)
	if err != nil {
		return assure.Decision{}, err
	}
	root, err := assure.RepositoryRoot(".")
	if err != nil {
		return assure.Decision{}, err
	}
	if lathe == "" {
		lathe = filepath.Join(root, "node_modules", ".bin", "lathe")
	}
	store, verifier, done, err := open(getenv)
	if err != nil {
		return assure.Decision{}, err
	}
	defer done()
	decision, _, err := assure.GateChange(assure.ChangeRequest{
		Root:      root,
		Commit:    commit,
		Base:      base,
		Trusted:   job.Trusted,
		ChangeRef: job.ChangeRef,
		Lathe:     lathe,
	}, store, verifier, assure.JobOutput{Repository: root, Out: stdout, Summary: getenv("GITHUB_STEP_SUMMARY")})
	return decision, err
}

// gateRelease gates the release commit in the repository that holds the working folder, reading it from the
// repository's git objects, never from the working tree. Before it gates, it refuses a step summary or a predicate
// inside the repository and a predicate file that is already there, so it writes no predicate but its own. The job's own workflow identity is the signer the
// attestation job signs a passed decision with.
func gateRelease(commit, predicate, lathe string, stdout io.Writer, getenv func(string) string, open provider) (assure.Decision, error) {
	job, err := assure.ReadGitHubGate(getenv)
	if err != nil {
		return assure.Decision{}, err
	}
	signer, err := assure.ReadGitHubRun(getenv)
	if err != nil {
		return assure.Decision{}, err
	}
	root, err := assure.RepositoryRoot(".")
	if err != nil {
		return assure.Decision{}, err
	}
	if lathe == "" {
		lathe = filepath.Join(root, "node_modules", ".bin", "lathe")
	}
	predicatePath, err := filepath.Abs(predicate)
	if err != nil {
		return assure.Decision{}, err
	}
	output := assure.JobOutput{Repository: root, Out: stdout, Summary: getenv("GITHUB_STEP_SUMMARY"), Predicate: predicatePath}
	if err := output.Check(); err != nil {
		return assure.Decision{}, err
	}
	store, verifier, done, err := open(getenv)
	if err != nil {
		return assure.Decision{}, err
	}
	defer done()
	decision, decided, err := assure.GateRelease(assure.ReleaseRequest{
		Root:    root,
		Commit:  commit,
		Trusted: job.Trusted,
		Signer:  signer.Signer,
		Lathe:   lathe,
	}, store, verifier, output)
	if err != nil {
		return assure.Decision{}, err
	}
	if decided.Attestation != nil {
		fmt.Fprintf(stdout, "decision predicate %s (%s), type %s, for %s to sign\n",
			predicatePath, decided.PredicateDigest, decided.Attestation.PredicateType, decided.Attestation.Signer.Subject)
	}
	return decision, nil
}

// gitHubProvider opens GitHub's attestation store, through the REST API with the job's token, and checks its
// bundles in process against the trust root of the public-good Sigstore instance, which TUF fetches into a new
// folder under the runner's temporary folder (or the system's) that it removes when done.
func gitHubProvider(getenv func(string) string) (assure.AttestationStore, assure.BundleVerifier, func(), error) {
	job, err := assure.ReadGitHubGate(getenv)
	if err != nil {
		return nil, nil, nil, err
	}
	temporary := getenv("RUNNER_TEMP")
	if temporary == "" {
		temporary = os.TempDir()
	}
	cache, err := os.MkdirTemp(temporary, "assure-sigstore-")
	if err != nil {
		return nil, nil, nil, err
	}
	done := func() { os.RemoveAll(cache) }
	trustedRoot, err := assure.PublicGoodTrustedRoot(cache)
	if err != nil {
		done()
		return nil, nil, nil, err
	}
	verifier, err := assure.NewSigstoreVerifier(trustedRoot)
	if err != nil {
		done()
		return nil, nil, nil, err
	}
	store := assure.GitHubAttestations{
		API:    job.API,
		Slug:   job.Slug,
		Token:  job.Token,
		Client: &http.Client{Timeout: time.Minute},
	}
	return store, verifier, done, nil
}
