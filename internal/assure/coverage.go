package assure

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Share names one of the three shares of the coverage (ST-ASR-TM08): designed, built or proved.
type Share string

// The three shares, in the order a decision names them.
const (
	Designed Share = "designed"
	Built    Share = "built"
	Proved   Share = "proved"
)

// allShares is the three shares in their fixed order.
func allShares() [3]Share {
	return [3]Share{Designed, Built, Proved}
}

// Coverage is how much of the spec's obligations Lathe counts as designed, built and proved at one commit
// (ST-VO55), summed over every domain and obligation kind.
type Coverage struct {
	Total    int
	Designed int
	Built    int
	Proved   int
}

// NewCoverage makes a coverage. It refuses a count below zero or above the total (ST-VO55-C01).
func NewCoverage(total, designed, built, proved int) (Coverage, error) {
	if total < 0 {
		return Coverage{}, fmt.Errorf("the coverage total %d is below zero", total)
	}
	coverage := Coverage{Total: total, Designed: designed, Built: built, Proved: proved}
	for _, share := range allShares() {
		if count := coverage.Count(share); count < 0 || count > total {
			return Coverage{}, fmt.Errorf("the %s count %d is not between zero and the total %d", share, count, total)
		}
	}
	return coverage, nil
}

// Count is the count of the share.
func (c Coverage) Count(share Share) int {
	switch share {
	case Designed:
		return c.Designed
	case Built:
		return c.Built
	case Proved:
		return c.Proved
	}
	return 0
}

// Ratio is the share: its count over the total. An empty spec counts as 100% (ST-VO55-C01).
func (c Coverage) Ratio(share Share) float64 {
	if c.Total == 0 {
		return 1
	}
	return float64(c.Count(share)) / float64(c.Total)
}

// String tells the total and each share, as "412 obligations: 412 designed (100.0%), ...". The same coverage
// always gives the same text.
func (c Coverage) String() string {
	parts := make([]string, 0, 3)
	for _, share := range allShares() {
		parts = append(parts, fmt.Sprintf("%d %s (%s%%)", c.Count(share), share, strconv.FormatFloat(100*c.Ratio(share), 'f', 1, 64)))
	}
	return fmt.Sprintf("%d obligations: %s", c.Total, strings.Join(parts, ", "))
}

// Standing is what Lathe counts one obligation as at a commit.
type Standing struct {
	Designed bool
	Built    bool
	Proved   bool
}

// Has tells if the obligation has the share.
func (s Standing) Has(share Share) bool {
	switch share {
	case Designed:
		return s.Designed
	case Built:
		return s.Built
	case Proved:
		return s.Proved
	}
	return false
}

// Measurement is the coverage at one commit, with the standing of each obligation by its ID.
type Measurement struct {
	Commit      string
	Coverage    Coverage
	Obligations map[string]Standing
}

// MeasureCoverage is the service of ST-ASR-DS02, Measure the coverage. It reads the coverage at the commit from
// lathe check on that commit's tree alone: it writes the tree of the commit from the git objects of the
// repository at root into a new folder, runs the Lathe CLI at the path lathe there, and reads the coverage and
// each obligation's chain from its JSON. It never reads the working tree. Two readings of one commit agree.
//
// A spec with an error does not give a coverage: when lathe check exits non-zero or names a problem of severity
// error, MeasureCoverage returns an error that names each problem, and no measurement. Lathe leaves the coverage
// of such a spec empty, and an empty coverage must never stand for the spec.
func MeasureCoverage(root, commit, lathe string) (Measurement, error) {
	if !fullCommit(commit) {
		return Measurement{}, fmt.Errorf("commit %q is not a full 40-character commit ID", commit)
	}
	tool, err := filepath.Abs(lathe)
	if err != nil {
		return Measurement{}, err
	}
	folder, err := os.MkdirTemp("", "assure-tree-")
	if err != nil {
		return Measurement{}, err
	}
	defer os.RemoveAll(folder)
	if err := writeTree(root, commit, folder); err != nil {
		return Measurement{}, err
	}
	command := exec.Command(tool, "check", "--json")
	command.Dir = folder
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	runErr := command.Run()
	if errs := latheErrors(stdout.Bytes()); len(errs) > 0 {
		return Measurement{}, &SpecError{Commit: commit, Problems: errs}
	}
	if runErr != nil {
		return Measurement{}, &SpecError{Commit: commit, Problems: []string{fmt.Sprintf("lathe check fails: %v: %s", runErr, latheSaid(stdout.Bytes(), stderr.Bytes()))}}
	}
	measurement, err := readLatheCheck(stdout.Bytes())
	if err != nil {
		return Measurement{}, fmt.Errorf("lathe check at %s: %w: %s", commit, err, strings.TrimSpace(stderr.String()))
	}
	measurement.Commit = commit
	return measurement, nil
}

// latheSaidLimit is the most bytes of Lathe's standard output that an error holds.
const latheSaidLimit = 4096

// latheSaid is what Lathe printed when it failed: its standard output, then its standard error, each trimmed and
// each only when it is not empty. Lathe can give its reason on either stream, such as "This product's data is for
// schema 2; this Lathe uses schema 5", so an error must hold both. Standard output longer than latheSaidLimit is
// cut there.
func latheSaid(stdout, stderr []byte) string {
	var said []string
	if out := strings.TrimSpace(string(stdout)); out != "" {
		if len(out) > latheSaidLimit {
			out = out[:latheSaidLimit] + " ..."
		}
		said = append(said, out)
	}
	if errText := strings.TrimSpace(string(stderr)); errText != "" {
		said = append(said, errText)
	}
	return strings.Join(said, "\n")
}

// SpecError tells that the coverage at a commit cannot be measured, because Lathe finds errors in the spec there
// or does not run. Problems names each error, one per line.
type SpecError struct {
	Commit   string
	Problems []string
}

func (e *SpecError) Error() string {
	return fmt.Sprintf("the coverage at %s cannot be measured: lathe check finds %d errors:\n%s",
		e.Commit, len(e.Problems), strings.Join(e.Problems, "\n"))
}

// latheErrors lists each problem of severity error in lathe check --json output, as "[check] record: message",
// in the order Lathe gives them. It lists none for output that is not JSON.
func latheErrors(data []byte) []string {
	var check struct {
		Products []struct {
			Workspace *struct {
				Problems []struct {
					Severity string `json:"severity"`
					Check    string `json:"check"`
					Record   string `json:"record"`
					Message  string `json:"message"`
				} `json:"problems"`
			} `json:"workspace"`
		} `json:"products"`
	}
	if json.Unmarshal(data, &check) != nil {
		return nil
	}
	var errs []string
	for _, product := range check.Products {
		if product.Workspace == nil {
			continue
		}
		for _, problem := range product.Workspace.Problems {
			if problem.Severity == "error" {
				errs = append(errs, fmt.Sprintf("[%s] %s: %s", problem.Check, problem.Record, strings.Join(strings.Fields(problem.Message), " ")))
			}
		}
	}
	return errs
}

// latheCheck is the part of lathe check --json that the coverage is read from.
type latheCheck struct {
	Products []struct {
		Workspace *struct {
			Coverage []struct {
				Obligations []struct {
					Kind     string `json:"kind"`
					Total    int    `json:"total"`
					Designed int    `json:"designed"`
					Built    int    `json:"built"`
					Proved   int    `json:"proved"`
				} `json:"obligations"`
				Gaps []struct {
					ID      string `json:"id"`
					Missing string `json:"missing"`
				} `json:"gaps"`
			} `json:"coverage"`
			Chains []struct {
				Obligation string `json:"obligation"`
			} `json:"chains"`
		} `json:"workspace"`
	} `json:"products"`
}

// readLatheCheck reads the coverage of the first product of lathe check --json, summed over every domain and
// obligation kind, and the standing of each obligation in its chains. An obligation is designed unless a gap
// says it is not, built when it is designed and no gap says it is not built, and proved unless a gap says it is
// not, as Lathe counts them. When the standings do not add up to Lathe's own counts, it refuses the output: a
// misread gap must never make an obligation look counted.
func readLatheCheck(data []byte) (Measurement, error) {
	var check latheCheck
	if err := json.Unmarshal(data, &check); err != nil {
		return Measurement{}, fmt.Errorf("the output is not lathe check --json: %w", err)
	}
	if len(check.Products) == 0 || check.Products[0].Workspace == nil {
		return Measurement{}, errors.New("the output names no product with a workspace")
	}
	workspace := check.Products[0].Workspace
	var total, designed, built, proved int
	missing := map[string]map[Share]bool{}
	for _, scope := range workspace.Coverage {
		for _, kind := range scope.Obligations {
			total += kind.Total
			designed += kind.Designed
			built += kind.Built
			proved += kind.Proved
		}
		for _, gap := range scope.Gaps {
			share := Share(gap.Missing)
			if share != Designed && share != Built && share != Proved {
				continue
			}
			if missing[gap.ID] == nil {
				missing[gap.ID] = map[Share]bool{}
			}
			missing[gap.ID][share] = true
		}
	}
	coverage, err := NewCoverage(total, designed, built, proved)
	if err != nil {
		return Measurement{}, err
	}
	obligations := make(map[string]Standing, len(workspace.Chains))
	for _, chain := range workspace.Chains {
		if chain.Obligation == "" {
			return Measurement{}, errors.New("a chain names no obligation")
		}
		if _, twice := obligations[chain.Obligation]; twice {
			return Measurement{}, fmt.Errorf("obligation %s has two chains", chain.Obligation)
		}
		gaps := missing[chain.Obligation]
		standing := Standing{Designed: !gaps[Designed], Proved: !gaps[Proved]}
		standing.Built = standing.Designed && !gaps[Built]
		obligations[chain.Obligation] = standing
	}
	for id := range missing {
		if _, ok := obligations[id]; !ok {
			return Measurement{}, fmt.Errorf("a gap names %s, which has no chain", id)
		}
	}
	counted := Coverage{Total: len(obligations)}
	for _, standing := range obligations {
		counted.Designed += count(standing.Designed)
		counted.Built += count(standing.Built)
		counted.Proved += count(standing.Proved)
	}
	if counted != coverage {
		return Measurement{}, fmt.Errorf("the chains and gaps give %s, but the coverage counts %s", counted, coverage)
	}
	return Measurement{Coverage: coverage, Obligations: obligations}, nil
}

// count is 1 for true and 0 for false.
func count(yes bool) int {
	if yes {
		return 1
	}
	return 0
}

// writeTree writes the tree of the commit, from the git objects of the repository at root, into the empty
// folder: every blob with its executable bit, and every symbolic link as a link. A submodule is an empty folder,
// as a checkout without its submodules has it. It reads the objects as they are, with no attributes, filters or
// replace refs, so the folder holds exactly the commit's tree. It writes only through the folder as an os.Root,
// and makes the links last, so no write follows a link. It refuses a link to an absolute path, and a link whose
// target leaves the tree, by its text or once the tree is written.
func writeTree(root, commit, folder string) error {
	listing, err := git(root, "ls-tree", "-r", "-z", "--full-tree", commit)
	if err != nil {
		return err
	}
	type entry struct {
		mode, kind, id, path string
	}
	var blobs []entry
	tree, err := os.OpenRoot(folder)
	if err != nil {
		return err
	}
	defer tree.Close()
	for _, line := range strings.Split(string(listing), "\x00") {
		if line == "" {
			continue
		}
		info, file, ok := strings.Cut(line, "\t")
		fields := strings.Fields(info)
		if !ok || len(fields) != 3 || !filepath.IsLocal(filepath.FromSlash(file)) {
			return fmt.Errorf("git ls-tree gives an entry that is not a file of the tree: %q", line)
		}
		e := entry{mode: fields[0], kind: fields[1], id: fields[2], path: file}
		switch e.kind {
		case "commit":
			if err := tree.MkdirAll(file, 0o755); err != nil {
				return err
			}
		case "blob":
			blobs = append(blobs, e)
		default:
			return fmt.Errorf("the tree of %s holds %s of kind %s", commit, e.path, e.kind)
		}
	}
	if len(blobs) == 0 {
		return nil
	}
	var ids strings.Builder
	for _, e := range blobs {
		ids.WriteString(e.id + "\n")
	}
	command := exec.Command("git", "-C", root, "cat-file", "--batch")
	command.Env = append(os.Environ(), "GIT_NO_REPLACE_OBJECTS=1")
	command.Stdin = strings.NewReader(ids.String())
	var stderr bytes.Buffer
	command.Stderr = &stderr
	out, err := command.StdoutPipe()
	if err != nil {
		return err
	}
	if err := command.Start(); err != nil {
		return err
	}
	reader := bufio.NewReader(out)
	links := map[string]string{}
	writeErr := func() error {
		for _, e := range blobs {
			header, err := reader.ReadString('\n')
			if err != nil {
				return err
			}
			fields := strings.Fields(header)
			if len(fields) != 3 || fields[0] != e.id || fields[1] != "blob" {
				return fmt.Errorf("git cat-file gives %q for blob %s", strings.TrimSpace(header), e.id)
			}
			size, err := strconv.ParseInt(fields[2], 10, 64)
			if err != nil {
				return err
			}
			content := make([]byte, size)
			if _, err := io.ReadFull(reader, content); err != nil {
				return err
			}
			if end, err := reader.ReadByte(); err != nil || end != '\n' {
				return fmt.Errorf("git cat-file does not end blob %s with a newline", e.id)
			}
			if e.mode == "120000" {
				links[e.path] = string(content)
				continue
			}
			if err := tree.MkdirAll(path.Dir(e.path), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(0o644)
			if e.mode == "100755" {
				mode = 0o755
			}
			if err := tree.WriteFile(e.path, content, mode); err != nil {
				return err
			}
		}
		return nil
	}()
	if writeErr != nil {
		_, _ = io.Copy(io.Discard, reader)
	}
	if err := command.Wait(); err != nil {
		return fmt.Errorf("git cat-file --batch: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	if writeErr != nil {
		return writeErr
	}
	return writeLinks(tree, folder, links)
}

// writeLinks makes each symbolic link of the tree, by its path, after every blob. It refuses an absolute target
// and a target that leaves the tree by its text. Once all links are made, it refuses a link that resolves to a
// place outside the folder through other links.
func writeLinks(tree *os.Root, folder string, links map[string]string) error {
	paths := make([]string, 0, len(links))
	for link := range links {
		paths = append(paths, link)
	}
	sort.Strings(paths)
	for _, link := range paths {
		target := links[link]
		if target == "" || path.IsAbs(target) || filepath.IsAbs(target) {
			return fmt.Errorf("the symbolic link %s points to the absolute path %q, outside the tree", link, target)
		}
		if resolved := path.Join(path.Dir(link), target); resolved == ".." || strings.HasPrefix(resolved, "../") {
			return fmt.Errorf("the symbolic link %s points to %q, outside the tree", link, target)
		}
		if err := tree.MkdirAll(path.Dir(link), 0o755); err != nil {
			return err
		}
		if err := tree.Symlink(target, link); err != nil {
			return err
		}
	}
	base, err := filepath.EvalSymlinks(folder)
	if err != nil {
		return err
	}
	for _, link := range paths {
		resolved, err := filepath.EvalSymlinks(filepath.Join(folder, filepath.FromSlash(link)))
		if err != nil {
			continue // A link to nothing points nowhere outside the tree.
		}
		if relative, err := filepath.Rel(base, resolved); err != nil || !filepath.IsLocal(relative) && relative != "." {
			return fmt.Errorf("the symbolic link %s resolves to %s, outside the tree", link, resolved)
		}
	}
	return nil
}

// sortedObligations lists the IDs of the measurement's obligations, in order.
func (m Measurement) sortedObligations() []string {
	ids := make([]string, 0, len(m.Obligations))
	for id := range m.Obligations {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
