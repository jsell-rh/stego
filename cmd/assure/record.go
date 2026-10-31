package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

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

func recordProofRun(commit, goJSON, pythonJSON, predicate string, stdout io.Writer, getenv func(string) string) error {
	job, err := assure.ReadGitHubRun(getenv)
	if err != nil {
		return err
	}
	root, err := git("rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	tree, err := checkout(root)
	if err != nil {
		return err
	}
	dataDir, err := specFolder(root)
	if err != nil {
		return err
	}
	spec, err := assure.ReadSpec(os.DirFS(root), dataDir)
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
	store := assure.GitHubAttestations{
		Repository: root,
		Predicate:  predicatePath,
		Outputs:    job.Outputs,
		Server:     job.Server,
		Name:       job.Name,
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
	}, store)
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

// checkout reads the commit the tree at root is at, and each change git status shows, untracked files too.
func checkout(root string) (assure.Checkout, error) {
	head, err := git("-C", root, "rev-parse", "HEAD")
	if err != nil {
		return assure.Checkout{}, err
	}
	status, err := gitRaw("-C", root, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return assure.Checkout{}, err
	}
	var changes []string
	for _, entry := range strings.Split(status, "\x00") {
		if entry != "" {
			changes = append(changes, entry)
		}
	}
	return assure.Checkout{Head: head, Changes: changes}, nil
}

// specFolder reads the folder of the spec's data files from lathe.config.json at root: its data key, or data.
func specFolder(root string) (string, error) {
	data, err := os.ReadFile(filepath.Join(root, "lathe.config.json"))
	if errors.Is(err, os.ErrNotExist) {
		return "data", nil
	}
	if err != nil {
		return "", err
	}
	var config struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return "", fmt.Errorf("lathe.config.json: %w", err)
	}
	if config.Data == "" {
		return "data", nil
	}
	return filepath.ToSlash(filepath.Clean(config.Data)), nil
}

func git(args ...string) (string, error) {
	out, err := gitRaw(args...)
	return strings.TrimSpace(out), err
}

func gitRaw(args ...string) (string, error) {
	command := exec.Command("git", args...)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	out, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}
