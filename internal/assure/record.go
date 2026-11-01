package assure

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Spec is what Assuring reads from the spec at a commit: the digest of its data files and its test records, which
// Record a proof run reads, and its domains by ID with their titles and its technical records that name a file or
// folder, which the code-standard predicates read.
type Spec struct {
	Digest    string
	Tests     map[string]TestRecord
	Domains   map[string]string
	Technical []TechnicalRecord
}

// TestRecord is one test record of the spec: its ID, the file that runs it, and whether its state is automated.
type TestRecord struct {
	ID        string
	File      string
	Automated bool
}

// automated lists the IDs of the automated test records, in order.
func (s Spec) automated() []string {
	var ids []string
	for id, record := range s.Tests {
		if record.Automated {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// SpecFile is one data file of the spec: its path from the repository's root, and its bytes.
type SpecFile struct {
	Path string
	Data []byte
}

// ReadSpec reads the spec's data files, the *.jsonl files of its data folder. Every *_test.jsonl file is a table
// of test records; a test record is automated when its state is automated. The domain table gives the domains,
// and the tables of the technical stage's kinds, Lathe's and the product's own, give the technical records that
// name a file or folder. The digest is "sha256:" and the
// SHA-256 of the lines "<SHA-256 of the file>  <path>", one per data file in path order, which is what
// sha256sum data/*.jsonl | sha256sum gives in the C locale.
func ReadSpec(files []SpecFile) (Spec, error) {
	if len(files) == 0 {
		return Spec{}, errors.New("the spec has no data files")
	}
	sorted := append([]SpecFile(nil), files...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	spec := Spec{Tests: make(map[string]TestRecord)}
	tables := make(map[string]SpecFile)
	var sums strings.Builder
	for _, file := range sorted {
		sum := sha256.Sum256(file.Data)
		fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(sum[:]), file.Path)
		if strings.HasSuffix(file.Path, "_test.jsonl") {
			if err := readTestRecords(file.Path, file.Data, spec.Tests); err != nil {
				return Spec{}, err
			}
			continue
		}
		tables[strings.TrimSuffix(path.Base(file.Path), ".jsonl")] = file
	}
	spec.Domains, spec.Technical = readTechnicalStage(tables)
	digest := sha256.Sum256([]byte(sums.String()))
	spec.Digest = "sha256:" + hex.EncodeToString(digest[:])
	return spec, nil
}

// ReadCommittedSpec reads the spec at the commit from the git objects of the repository at root, never from the
// working tree: the data folder that lathe.config.json names at the commit (data when it names none), and every
// *.jsonl file directly in it.
func ReadCommittedSpec(root, commit string) (Spec, error) {
	dir := "data"
	listed, err := git(root, "ls-tree", "-z", "--name-only", commit, "--", "lathe.config.json")
	if err != nil {
		return Spec{}, err
	}
	if len(listed) > 0 {
		data, err := git(root, "cat-file", "blob", commit+":lathe.config.json")
		if err != nil {
			return Spec{}, err
		}
		var config struct {
			Data string `json:"data"`
		}
		if err := json.Unmarshal(data, &config); err != nil {
			return Spec{}, fmt.Errorf("lathe.config.json at %s: %w", commit, err)
		}
		if config.Data != "" {
			dir = path.Clean(config.Data)
		}
	}
	tree, err := git(root, "ls-tree", "-z", commit, "--", dir+"/")
	if err != nil {
		return Spec{}, err
	}
	var files []SpecFile
	for _, entry := range strings.Split(string(tree), "\x00") {
		info, file, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(info)
		if !ok || len(fields) != 3 || fields[1] != "blob" || path.Dir(file) != dir || !strings.HasSuffix(file, ".jsonl") {
			continue
		}
		data, err := git(root, "cat-file", "blob", fields[2])
		if err != nil {
			return Spec{}, err
		}
		files = append(files, SpecFile{Path: file, Data: data})
	}
	if len(files) == 0 {
		return Spec{}, fmt.Errorf("the spec at %s has no data files in %s", commit, dir)
	}
	return ReadSpec(files)
}

// RepositoryRoot is the root of the git repository that holds the folder dir.
func RepositoryRoot(dir string) (string, error) {
	out, err := git(dir, "rev-parse", "--show-toplevel")
	return strings.TrimSpace(string(out)), err
}

// ReadCheckout reads the state of the tree at root: the commit checked out, and each change that is not committed.
// A change is:
//   - any entry git status shows, untracked files too, with no excludes file but the committed .gitignore files,
//     and with full stat checks, so that core.checkStat, core.trustctime, core.fileMode, fsmonitor or the untracked
//     cache in the repository's configuration hide nothing;
//   - any file whose index entry hides its changes from git status (assume-unchanged or skip-worktree, as git
//     ls-files -v shows them);
//   - any pattern in the repository's info/exclude file;
//   - any untracked .gitignore file that ignores itself, which can hide new files next to it (one in a folder
//     that is ignored as a whole, such as node_modules, can hide nothing more and is not counted);
//   - any file of the commit's tree whose bytes, kind or executable bit in the working tree differ from the commit,
//     compared by object ID with no filters, so that no stat setting can hide it.
//
// Every git call runs with GIT_NO_REPLACE_OBJECTS=1, so a replace ref cannot show another commit's tree under the
// commit's ID.
//
// These checks catch accidents and the ordinary ways to hide a change from git. They cannot stop code that runs
// in the same job before assure record: such code can always defeat a check made on the job's own disk. What
// keeps the subject honest is the proof run workflow, whose attest job runs no code of the repository and names
// the commit itself.
func ReadCheckout(root string) (Checkout, error) {
	head, err := git(root, "rev-parse", "HEAD")
	if err != nil {
		return Checkout{}, err
	}
	status, err := git(root, "-c", "core.excludesFile="+os.DevNull, "-c", "core.fsmonitor=false", "-c", "core.untrackedCache=false",
		"-c", "core.checkStat=default", "-c", "core.trustctime=true", "-c", "core.fileMode=true",
		"status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil {
		return Checkout{}, err
	}
	var changes []string
	for _, entry := range strings.Split(string(status), "\x00") {
		if entry != "" {
			changes = append(changes, entry)
		}
	}
	index, err := git(root, "ls-files", "-v", "-z")
	if err != nil {
		return Checkout{}, err
	}
	for _, entry := range strings.Split(string(index), "\x00") {
		if len(entry) > 2 && entry[:2] != "H " {
			changes = append(changes, entry[2:]+" is hidden from git status (git ls-files -v tag "+entry[:1]+")")
		}
	}
	exclude, err := git(root, "rev-parse", "--git-path", "info/exclude")
	if err != nil {
		return Checkout{}, err
	}
	excludeFile := strings.TrimSpace(string(exclude))
	if !filepath.IsAbs(excludeFile) {
		excludeFile = filepath.Join(root, excludeFile)
	}
	patterns, err := os.ReadFile(excludeFile)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Checkout{}, err
	}
	for _, line := range strings.Split(string(patterns), "\n") {
		if pattern := strings.TrimSpace(line); pattern != "" && !strings.HasPrefix(pattern, "#") {
			changes = append(changes, "info/exclude hides "+pattern)
		}
	}
	ignored, err := git(root, "-c", "core.excludesFile="+os.DevNull, "ls-files", "-z", "--others", "--ignored", "--exclude-standard", "--directory")
	if err != nil {
		return Checkout{}, err
	}
	for _, file := range strings.Split(string(ignored), "\x00") {
		if path.Base(file) == ".gitignore" {
			changes = append(changes, file+" is an untracked .gitignore file that git ignores")
		}
	}
	differ, err := treeDifferences(root)
	if err != nil {
		return Checkout{}, err
	}
	changes = append(changes, differ...)
	return Checkout{Head: strings.TrimSpace(string(head)), Changes: changes}, nil
}

// treeDifferences lists each file of HEAD's tree that the working tree does not hold as committed: missing, of
// another kind, with another executable bit, or with other bytes. It hashes the working file as git hashes a blob,
// with no filters, so no stat setting or index flag can hide a change.
func treeDifferences(root string) ([]string, error) {
	tree, err := git(root, "ls-tree", "-r", "-z", "--full-tree", "HEAD")
	if err != nil {
		return nil, err
	}
	var differ []string
	for _, entry := range strings.Split(string(tree), "\x00") {
		info, file, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(info)
		if !ok || len(fields) != 3 || fields[1] != "blob" {
			continue
		}
		mode, id := fields[0], fields[2]
		got, err := workingObjectID(filepath.Join(root, filepath.FromSlash(file)), mode, len(id))
		if err != nil {
			differ = append(differ, file+" differs from the commit: "+err.Error())
			continue
		}
		if got != id {
			differ = append(differ, file+" differs from the commit")
		}
	}
	return differ, nil
}

// workingObjectID is the git object ID of the working file as a blob of the mode given: the link's target for a
// symbolic link, else the file's bytes. hexLength tells the repository's object format: 40 for SHA-1, 64 for
// SHA-256. It refuses a file of another kind or another executable bit than the mode.
func workingObjectID(file, mode string, hexLength int) (string, error) {
	info, err := os.Lstat(file)
	if err != nil {
		return "", err
	}
	var content io.Reader
	var size int64
	switch {
	case mode == "120000":
		if info.Mode()&os.ModeSymlink == 0 {
			return "", errors.New("it is not a symbolic link")
		}
		target, err := os.Readlink(file)
		if err != nil {
			return "", err
		}
		content, size = strings.NewReader(target), int64(len(target))
	case !info.Mode().IsRegular():
		return "", errors.New("it is not a regular file")
	case (mode == "100755") != (info.Mode().Perm()&0o100 != 0):
		return "", errors.New("its executable bit differs")
	default:
		opened, err := os.Open(file)
		if err != nil {
			return "", err
		}
		defer opened.Close()
		content, size = opened, info.Size()
	}
	var hasher hash.Hash
	switch hexLength {
	case 40:
		hasher = sha1.New()
	case 64:
		hasher = sha256.New()
	default:
		return "", fmt.Errorf("object IDs of %d digits are not SHA-1 or SHA-256", hexLength)
	}
	fmt.Fprintf(hasher, "blob %d\x00", size)
	written, err := io.Copy(hasher, content)
	if err != nil {
		return "", err
	}
	if written != size {
		return "", errors.New("it changed while it was read")
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// git runs git in the repository at root and returns what it writes to its standard output. Replace refs are
// off, so every object is the one its ID names.
func git(root string, args ...string) ([]byte, error) {
	command := exec.Command("git", append([]string{"-C", root}, args...)...)
	command.Env = append(os.Environ(), "GIT_NO_REPLACE_OBJECTS=1")
	var stderr bytes.Buffer
	command.Stderr = &stderr
	out, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// readTestRecords reads one table of test records, one JSON object per line, into tests.
func readTestRecords(file string, data []byte, tests map[string]TestRecord) error {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			continue
		}
		var row struct {
			ID    string `json:"id"`
			File  string `json:"file"`
			State string `json:"state"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			return fmt.Errorf("%s line %d: %w", file, line, err)
		}
		if row.ID == "" {
			return fmt.Errorf("%s line %d: a test record has no ID", file, line)
		}
		if _, ok := tests[row.ID]; ok {
			return fmt.Errorf("%s line %d: test record %s is in the spec twice", file, line, row.ID)
		}
		tests[row.ID] = TestRecord{ID: row.ID, File: row.File, Automated: row.State == "automated"}
	}
	return scanner.Err()
}

// RecordRequest is what Record a proof run reads: the commit CI checked out and the state of its tree, the spec
// at that commit, the suites' output, and the CI run with the identity its attestation action signs with.
type RecordRequest struct {
	Commit       string
	Tree         Checkout
	Spec         Spec
	GoTestJSON   io.Reader
	PythonJSON   io.Reader
	Provider     string
	RunReference string
	Signer       Signer
	Trusted      TrustedSigner
}

// RecordProofRun is the handler of ST-ASR-CMD01, Record a proof run. It builds the proof run from the suites'
// output at the commit CI checked out, fills in a failed outcome for every automated test record that did not
// run, refuses an outcome for an unknown ID, and hands the statement to the store for the provider to attest.
// When it refuses, the store gets nothing.
func RecordProofRun(request RecordRequest, store AttestationStore) (ProofRun, ProofRunAttested, error) {
	if err := atOneCleanCommit(request.Commit, request.Tree); err != nil {
		return ProofRun{}, ProofRunAttested{}, err
	}
	if !knownProvider(request.Provider) {
		return ProofRun{}, ProofRunAttested{}, fmt.Errorf("CI provider %q is not one of github-actions or konflux", request.Provider)
	}
	if request.RunReference == "" {
		return ProofRun{}, ProofRunAttested{}, errors.New("the proof run has no run reference")
	}
	suites, err := ReadOutcomes(request.GoTestJSON, request.PythonJSON)
	if err != nil {
		return ProofRun{}, ProofRunAttested{}, err
	}
	if err := outcomesNameKnownRecords(suites.Outcomes, request.Spec); err != nil {
		return ProofRun{}, ProofRunAttested{}, err
	}
	outcomes := outcomeForEveryAutomatedTest(suites.Outcomes, request.Spec)
	for i := range outcomes {
		outcomes[i].File = request.Spec.Tests[outcomes[i].TestRecord].File
	}
	run := ProofRun{
		Commit:       request.Commit,
		Provider:     request.Provider,
		RunReference: request.RunReference,
		StartedAt:    suites.StartedAt,
		FinishedAt:   suites.FinishedAt,
		SpecDigest:   request.Spec.Digest,
		Outcomes:     outcomes,
	}
	attestation, err := NewAttestation(ProofRunPredicateType, request.Commit, request.Signer, "")
	if err != nil {
		return ProofRun{}, ProofRunAttested{}, err
	}
	if err := attestedByTrustedSigner(run, attestation, request.Trusted); err != nil {
		return ProofRun{}, ProofRunAttested{}, err
	}
	run.Attestation = attestation
	digest, err := store.HandOver(run)
	if err != nil {
		return ProofRun{}, ProofRunAttested{}, err
	}
	return run, ProofRunAttested{
		Commit:          run.Commit,
		SpecDigest:      run.SpecDigest,
		Attestation:     attestation,
		PredicateDigest: digest,
	}, nil
}

// knownProvider tells if the provider is a CI provider (ST-VO51): github-actions or konflux.
func knownProvider(provider string) bool {
	switch provider {
	case "github-actions", "konflux":
		return true
	}
	return false
}
