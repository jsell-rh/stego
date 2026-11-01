package assure

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
// Fetch reads the attestations GitHub keeps for a commit from the REST API at API (GITHUB_API_URL), for the
// repository Slug (owner/name), with Token when there is one. It only moves bytes: VerifyAttestation checks
// each bundle before any gate reads it.
type GitHubAttestations struct {
	Repository string
	Predicate  string
	API        string
	Slug       string
	Token      string
	Client     *http.Client
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

// maxAttestationPages is how many pages of attestations Fetch reads for one commit, at 100 to a page.
const maxAttestationPages = 10

// Fetch returns the bundle of each proof run attestation GitHub keeps under SubjectDigest of the commit, in the
// order GitHub gives them. GitHub answers 404 when it keeps none; Fetch then returns none. An attestation whose
// bundle GitHub does not give inline gives an empty bundle, which no check accepts.
func (g GitHubAttestations) Fetch(commit string) ([][]byte, error) {
	if !fullCommit(commit) {
		return nil, fmt.Errorf("commit %q is not a full 40-character commit ID", commit)
	}
	api, err := url.Parse(strings.TrimSuffix(g.API, "/"))
	if err != nil || api.Scheme != "https" || api.Host == "" {
		return nil, fmt.Errorf("the GitHub API URL %q is not an https URL", g.API)
	}
	if !validSlug(g.Slug) {
		return nil, fmt.Errorf("the GitHub repository %q is not owner/name", g.Slug)
	}
	client := g.Client
	if client == nil {
		client = http.DefaultClient
	}
	query := url.Values{"per_page": {"100"}, "predicate_type": {ProofRunPredicateType}}
	next := api.String() + "/repos/" + g.Slug + "/attestations/" + SubjectDigest(commit) + "?" + query.Encode()
	var bundles [][]byte
	for page := 0; next != ""; page++ {
		if page == maxAttestationPages {
			return nil, fmt.Errorf("GitHub keeps more than %d pages of attestations for %s", maxAttestationPages, commit)
		}
		request, err := http.NewRequest(http.MethodGet, next, nil)
		if err != nil {
			return nil, err
		}
		request.Header.Set("Accept", "application/vnd.github+json")
		request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		if g.Token != "" {
			request.Header.Set("Authorization", "Bearer "+g.Token)
		}
		response, err := client.Do(request)
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(io.LimitReader(response.Body, 64<<20))
		response.Body.Close()
		if err != nil {
			return nil, err
		}
		if response.StatusCode == http.StatusNotFound && page == 0 {
			return nil, nil
		}
		if response.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GitHub answers %s for the attestations of %s: %s", response.Status, commit, strings.TrimSpace(string(body)))
		}
		var listing struct {
			Attestations []struct {
				Bundle json.RawMessage `json:"bundle"`
			} `json:"attestations"`
		}
		if err := json.Unmarshal(body, &listing); err != nil {
			return nil, fmt.Errorf("GitHub's attestations of %s: %w", commit, err)
		}
		for _, attestation := range listing.Attestations {
			bundle := []byte(attestation.Bundle)
			if string(bundle) == "null" {
				bundle = nil
			}
			bundles = append(bundles, bundle)
		}
		next, err = nextPage(response.Header.Get("Link"), api)
		if err != nil {
			return nil, err
		}
	}
	return bundles, nil
}

// validSlug tells if the text is a GitHub repository's owner/name.
func validSlug(slug string) bool {
	owner, name, ok := strings.Cut(slug, "/")
	if !ok || owner == "" || name == "" || name == "." || name == ".." {
		return false
	}
	for _, r := range owner + name {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' && r != '.' {
			return false
		}
	}
	return true
}

// nextPage reads the next page's URL from a Link header, and refuses one on another host than the API's.
func nextPage(link string, api *url.URL) (string, error) {
	for _, part := range strings.Split(link, ",") {
		target, params, ok := strings.Cut(strings.TrimSpace(part), ";")
		if !ok || !strings.Contains(params, `rel="next"`) {
			continue
		}
		next, err := url.Parse(strings.Trim(strings.TrimSpace(target), "<>"))
		if err != nil {
			return "", err
		}
		if next.Scheme != api.Scheme || next.Host != api.Host {
			return "", fmt.Errorf("GitHub's next page %s is not at %s", next, api.Host)
		}
		return next.String(), nil
	}
	return "", nil
}

// GitHubGate is what a GitHub Actions job of a pull request says to Gate a change: the trusted signer of a proof
// run in its repository, the change's own reference, at which the proof run workflow of the pull request signs,
// and where and how to read the attestations.
type GitHubGate struct {
	Trusted   TrustedSigner
	ChangeRef string
	API       string
	Slug      string
	Token     string
}

// ReadGitHubGate reads the job's environment through getenv. GITHUB_REF is the change's own reference, such as
// refs/pull/7/merge; with none, only a proof run signed at main counts. GITHUB_API_URL is https://api.github.com
// when the job does not set it.
func ReadGitHubGate(getenv func(string) string) (GitHubGate, error) {
	server, slug := strings.TrimSuffix(getenv("GITHUB_SERVER_URL"), "/"), getenv("GITHUB_REPOSITORY")
	if server == "" || slug == "" {
		return GitHubGate{}, errors.New("GITHUB_SERVER_URL and GITHUB_REPOSITORY, which name the trusted signer's repository, are not set")
	}
	api := getenv("GITHUB_API_URL")
	if api == "" {
		api = "https://api.github.com"
	}
	return GitHubGate{
		Trusted:   TrustedSigner{Issuer: GitHubIssuer, Workflow: server + "/" + slug + "/" + ProofRunWorkflow},
		ChangeRef: getenv("GITHUB_REF"),
		API:       api,
		Slug:      slug,
		Token:     getenv("GITHUB_TOKEN"),
	}, nil
}

// GitHubRun is what a GitHub Actions job says about itself: the provider, the run and attempt, the identity its
// attestation action signs with, and the trusted signer of a proof run in its repository.
type GitHubRun struct {
	Provider     string
	RunReference string
	Signer       Signer
	Trusted      TrustedSigner
}

// ReadGitHubRun reads the job's environment through getenv. It refuses outside a GitHub Actions job. assure
// record and assure gate release read it for the identity their job's attestation action signs with.
func ReadGitHubRun(getenv func(string) string) (GitHubRun, error) {
	if getenv("GITHUB_ACTIONS") != "true" {
		return GitHubRun{}, errors.New("assure record and assure gate release run only in a GitHub Actions job: GITHUB_ACTIONS is not true")
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
