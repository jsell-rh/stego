package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/jsell-rh/stego/internal/assure"
)

// record is assure record (ST-ASR-CLI01). It reads the suites' output, writes the proof run predicate for the
// provider's attestation action, and exits non-zero on an unknown test ID, a commit that is not the full commit
// checked out, or a tree with uncommitted changes. When it refuses, it writes nothing.
func record(args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	flags := flag.NewFlagSet("assure record", flag.ContinueOnError)
	flags.SetOutput(stderr)
	commit := flags.String("commit", "", "the full commit CI checked out")
	goJSON := flags.String("go-json", "", "the go test -json output")
	pythonJSON := flags.String("python-json", "", "the output of scripts/run-python-tests.py")
	predicate := flags.String("predicate", "", "the predicate file to write, outside the repository")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 || *commit == "" || *goJSON == "" || *pythonJSON == "" || *predicate == "" {
		fmt.Fprintln(stderr, "assure record: --commit, --go-json, --python-json and --predicate are required, and nothing else")
		return 2
	}
	if err := recordProofRun(*commit, *goJSON, *pythonJSON, *predicate, stdout, getenv); err != nil {
		fmt.Fprintf(stderr, "assure record: %v\n", err)
		return 1
	}
	return 0
}

// recordProofRun reads the tree of the repository that holds the working folder, and the spec from the git
// objects of the commit checked out, never from the working tree.
func recordProofRun(commit, goJSON, pythonJSON, predicate string, stdout io.Writer, getenv func(string) string) error {
	job, err := assure.ReadGitHubRun(getenv)
	if err != nil {
		return err
	}
	root, err := assure.RepositoryRoot(".")
	if err != nil {
		return err
	}
	tree, err := assure.ReadCheckout(root)
	if err != nil {
		return err
	}
	spec, err := assure.ReadCommittedSpec(root, tree.Head)
	if err != nil {
		return err
	}
	goFile, err := os.Open(goJSON)
	if err != nil {
		return err
	}
	defer goFile.Close()
	pythonFile, err := os.Open(pythonJSON)
	if err != nil {
		return err
	}
	defer pythonFile.Close()
	predicatePath, err := filepath.Abs(predicate)
	if err != nil {
		return err
	}
	run, attested, err := assure.RecordProofRun(assure.RecordRequest{
		Commit:       commit,
		Tree:         tree,
		Spec:         spec,
		GoTestJSON:   goFile,
		PythonJSON:   pythonFile,
		Provider:     job.Provider,
		RunReference: job.RunReference,
		Signer:       job.Signer,
		Trusted:      job.Trusted,
	}, assure.GitHubAttestations{Repository: root, Predicate: predicatePath})
	if err != nil {
		return err
	}
	counts := map[assure.Result]int{}
	for _, outcome := range run.Outcomes {
		counts[outcome.Result]++
	}
	fmt.Fprintf(stdout, "proof run at %s: %d outcomes, %d passed, %d failed, %d skipped\n",
		run.Commit, len(run.Outcomes), counts[assure.Passed], counts[assure.Failed], counts[assure.Skipped])
	fmt.Fprintf(stdout, "predicate %s (%s), type %s, for %s to sign\n",
		predicatePath, attested.PredicateDigest, attested.Attestation.PredicateType, attested.Attestation.Signer.Subject)
	return nil
}
