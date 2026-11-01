package assure

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// GitHubIssuer is the OIDC issuer that vouches for a GitHub Actions workflow's keyless identity.
const GitHubIssuer = "https://token.actions.githubusercontent.com"

// ProofRunWorkflow is the workflow whose keyless identity is GitHub Actions' trusted signer of a proof run, from
// the repository's root.
const ProofRunWorkflow = ".github/workflows/proof-run.yml"

// GitHubAttestations is the adapter to GitHub Actions (ST-INT01-AD). HandOver writes the proof run's predicate to
// the file Predicate, outside the product's repository at Repository. The proof run workflow uploads the file,
// and a second job, which runs no code of the repository, names the commit as the subject (by SubjectDigest) and
// has actions/attest sign the predicate.
//
// Fetching and verifying the attestations for a commit through the GitHub attestations API belongs to this
// adapter too. Gate a change (ST-ASR-UC02) adds it with ST-ASR-DS03.
type GitHubAttestations struct {
	Repository string
	Predicate  string
}

// HandOver writes the proof run's predicate and returns its digest.
func (g GitHubAttestations) HandOver(run ProofRun) (string, error) {
	data, err := encodeProofRun(run)
	if err != nil {
		return "", err
	}
	if err := writeOutside(g.Repository, g.Predicate, data); err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// GitHubRun is what a GitHub Actions job says about itself: the provider, the run and attempt, the identity its
// attestation action signs with, and the trusted signer of a proof run in its repository.
type GitHubRun struct {
	Provider     string
	RunReference string
	Signer       Signer
	Trusted      TrustedSigner
}

// ReadGitHubRun reads the job's environment through getenv. It refuses outside a GitHub Actions job.
func ReadGitHubRun(getenv func(string) string) (GitHubRun, error) {
	if getenv("GITHUB_ACTIONS") != "true" {
		return GitHubRun{}, errors.New("assure record runs only in a GitHub Actions job: GITHUB_ACTIONS is not true")
	}
	values := map[string]string{}
	for _, name := range []string{"GITHUB_RUN_ID", "GITHUB_RUN_ATTEMPT", "GITHUB_SERVER_URL", "GITHUB_REPOSITORY", "GITHUB_WORKFLOW_REF"} {
		value := getenv(name)
		if value == "" {
			return GitHubRun{}, fmt.Errorf("the GitHub Actions job does not set %s", name)
		}
		values[name] = value
	}
	server := strings.TrimSuffix(values["GITHUB_SERVER_URL"], "/")
	return GitHubRun{
		Provider:     "github-actions",
		RunReference: "github:" + values["GITHUB_RUN_ID"] + "/" + values["GITHUB_RUN_ATTEMPT"],
		Signer:       Signer{Issuer: GitHubIssuer, Subject: server + "/" + values["GITHUB_WORKFLOW_REF"]},
		Trusted:      TrustedSigner{Issuer: GitHubIssuer, Workflow: server + "/" + values["GITHUB_REPOSITORY"] + "/" + ProofRunWorkflow},
	}, nil
}
