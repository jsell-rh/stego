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

// gate is assure gate change (ST-ASR-CLI02). It gates the change's commit against the base head, prints the
// decision with every reason, and exits 1 when the change is refused. When it cannot decide, such as when Lathe
// finds an error in the spec at either commit, it prints no decision, names why, and exits 2.
func gate(args []string, stdout, stderr io.Writer, getenv func(string) string, open provider) int {
	if len(args) == 0 || args[0] != "change" {
		fmt.Fprintln(stderr, "assure gate: the gate is change: assure gate change --commit <sha> --base <sha>")
		return 2
	}
	flags := flag.NewFlagSet("assure gate change", flag.ContinueOnError)
	flags.SetOutput(stderr)
	commit := flags.String("commit", "", "the full commit of the change")
	base := flags.String("base", "", "the full commit at the head of the branch the change merges into")
	lathe := flags.String("lathe", "", "the Lathe CLI (default node_modules/.bin/lathe of the repository)")
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}
	if flags.NArg() > 0 || *commit == "" || *base == "" {
		fmt.Fprintln(stderr, "assure gate change: --commit and --base are required, and nothing else but --lathe")
		return 2
	}
	decision, err := gateChange(*commit, *base, *lathe, stdout, getenv, open)
	if err != nil {
		fmt.Fprintf(stderr, "assure gate change: no decision: %v\n", err)
		return 2
	}
	if !decision.Passed {
		return 1
	}
	return 0
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
