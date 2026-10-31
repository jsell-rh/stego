package assure

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

// SuiteOutcomes is what the suites' output showed: one outcome per test record ID that a test's name carries, in
// test record ID order, and when the first and the last test event happened.
type SuiteOutcomes struct {
	Outcomes   []Outcome
	StartedAt  time.Time
	FinishedAt time.Time
}

// maxDetail is the most bytes of a test's output that an outcome keeps as its detail.
const maxDetail = 4000

// ReadOutcomes is the domain service ST-ASR-DS01, Read the test outcomes. It reads the test runners' own
// machine-readable output: the events of go test -json for Go, and the document of scripts/run-python-tests.py,
// which writes the unittest result, for Python. Each test and subtest whose own name carries a test record's ID
// gives an outcome for each ID it carries; a test whose name carries no ID gives no outcome. A test record's ID
// that runs more than once gets one outcome (ST-ASR-INV06). The outcomes are in test record ID order.
//
// A word of a name that starts with a test record's ID and goes on with a hyphen or a lower-case letter, such as
// ST-FIX-INV01-T-holds or ST-FIX-INV01-Tx, carries no ID, but its test surely meant to carry one. ReadOutcomes
// refuses the output and names the word, so the name is fixed, and the test record does not show as not run.
func ReadOutcomes(goTestJSON, pythonJSON io.Reader) (SuiteOutcomes, error) {
	var bounds timeBounds
	goRuns, err := readGoTestEvents(goTestJSON, &bounds)
	if err != nil {
		return SuiteOutcomes{}, fmt.Errorf("go test -json output: %w", err)
	}
	pythonRuns, err := readPythonResults(pythonJSON, &bounds)
	if err != nil {
		return SuiteOutcomes{}, fmt.Errorf("Python test results: %w", err)
	}
	if bounds.first.IsZero() {
		return SuiteOutcomes{}, errors.New("the suites' output holds no timed test event")
	}
	return SuiteOutcomes{
		Outcomes:   oneOutcomePerTestRecord(append(goRuns, pythonRuns...)),
		StartedAt:  bounds.first.UTC(),
		FinishedAt: bounds.last.UTC(),
	}, nil
}

// timeBounds holds the first and the last time the suites' output gives.
type timeBounds struct {
	first, last time.Time
}

func (b *timeBounds) add(t time.Time) {
	if t.IsZero() {
		return
	}
	if b.first.IsZero() || t.Before(b.first) {
		b.first = t
	}
	if b.last.IsZero() || t.After(b.last) {
		b.last = t
	}
}

// goTestEvent is one line of go test -json output, as cmd/test2json writes it.
type goTestEvent struct {
	Time    time.Time
	Action  string
	Package string
	Test    string
	Elapsed *float64
	Output  string
}

// readGoTestEvents reads go test -json output. A test's own name is its full name without its parent test's
// full name, so a subtest does not carry the IDs of the test that holds it.
func readGoTestEvents(r io.Reader, bounds *timeBounds) ([]testRun, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	known := make(map[string]map[string]bool) // package -> full test names seen
	output := make(map[string]*strings.Builder)
	var runs []testRun
	line := 0
	for scanner.Scan() {
		line++
		text := bytes.TrimSpace(scanner.Bytes())
		if len(text) == 0 {
			continue
		}
		var event goTestEvent
		if err := json.Unmarshal(text, &event); err != nil {
			return nil, fmt.Errorf("line %d is not a test event: %w", line, err)
		}
		bounds.add(event.Time)
		if event.Test == "" {
			continue
		}
		key := event.Package + "\x00" + event.Test
		switch event.Action {
		case "run":
			if known[event.Package] == nil {
				known[event.Package] = make(map[string]bool)
			}
			known[event.Package][event.Test] = true
		case "output":
			if output[key] == nil {
				output[key] = &strings.Builder{}
			}
			output[key].WriteString(event.Output)
		case "pass", "fail", "skip":
			result := map[string]Result{"pass": Passed, "fail": Failed, "skip": Skipped}[event.Action]
			detail := ""
			if result != Passed && output[key] != nil {
				detail = goDetail(output[key].String())
			}
			ids, err := testRecordIDs(ownName(event.Test, known[event.Package]))
			if err != nil {
				return nil, fmt.Errorf("test %s: %w", event.Test, err)
			}
			for _, id := range ids {
				run := testRun{id: id, result: result, detail: detail}
				if event.Elapsed != nil {
					run.duration, run.timed = *event.Elapsed, true
				}
				runs = append(runs, run)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return runs, nil
}

// ownName is a Go test's name without the full name of the test that holds it: the longest test seen to run
// whose full name, and a slash, start this one.
func ownName(full string, known map[string]bool) string {
	parent := ""
	for name := range known {
		if len(name) > len(parent) && strings.HasPrefix(full, name+"/") {
			parent = name
		}
	}
	if parent == "" {
		return full
	}
	return full[len(parent)+1:]
}

// goDetail is a failed or skipped Go test's output without the lines go test frames it with, cut to maxDetail
// bytes.
func goDetail(output string) string {
	var kept []string
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "=== ") || strings.HasPrefix(trimmed, "--- ") {
			continue
		}
		kept = append(kept, trimmed)
	}
	return cut(strings.Join(kept, "\n"))
}

// cut shortens text to at most maxDetail bytes, on a character boundary.
func cut(text string) string {
	if len(text) <= maxDetail {
		return text
	}
	end := maxDetail
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end]
}

// pythonResults is the document scripts/run-python-tests.py writes: format 1, when the suites started and
// finished, and one entry per test and subtest. Name is the test's own name: a test method's name, or a
// subtest's description.
type pythonResults struct {
	Format   int          `json:"format"`
	Started  time.Time    `json:"started"`
	Finished time.Time    `json:"finished"`
	Tests    []pythonTest `json:"tests"`
}

type pythonTest struct {
	Test    string   `json:"test"`
	Name    string   `json:"name"`
	Result  string   `json:"result"`
	Elapsed *float64 `json:"elapsed"`
	Detail  string   `json:"detail"`
}

// pythonFormat is the version of the document scripts/run-python-tests.py writes.
const pythonFormat = 1

func readPythonResults(r io.Reader, bounds *timeBounds) ([]testRun, error) {
	decoder := json.NewDecoder(r)
	decoder.DisallowUnknownFields()
	var document pythonResults
	if err := decoder.Decode(&document); err != nil {
		return nil, err
	}
	if document.Format != pythonFormat {
		return nil, fmt.Errorf("format %d is not format %d", document.Format, pythonFormat)
	}
	bounds.add(document.Started)
	bounds.add(document.Finished)
	var runs []testRun
	for _, test := range document.Tests {
		result, err := ParseResult(test.Result)
		if err != nil {
			return nil, fmt.Errorf("test %s: %w", test.Test, err)
		}
		detail := ""
		if result != Passed {
			detail = cut(strings.TrimSpace(test.Detail))
		}
		ids, err := testRecordIDs(test.Name)
		if err != nil {
			return nil, fmt.Errorf("test %s: %w", test.Test, err)
		}
		for _, id := range ids {
			run := testRun{id: id, result: result, detail: detail}
			if test.Elapsed != nil {
				run.duration, run.timed = *test.Elapsed, true
			}
			runs = append(runs, run)
		}
	}
	return runs, nil
}

// testRecordIDs lists the test record IDs a test's own name carries, each once, in the order the name gives
// them. The name is cut into words at every character that is not a letter, a digit or a hyphen, so the spaces
// that go test writes as underscores also part words. A word is an ID when it matches
// ST-[A-Z0-9]+(-[A-Z0-9]+)*-T[0-9]*. It refuses a name with a word that malformedID finds.
func testRecordIDs(name string) ([]string, error) {
	words := strings.FieldsFunc(name, func(r rune) bool {
		return !(r == '-' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	})
	var ids []string
	seen := make(map[string]bool)
	for _, word := range words {
		if malformedID(word) {
			return nil, fmt.Errorf("%q starts with a test record ID and goes on, so it is not one; put a space after the ID", word)
		}
		if isTestRecordID(word) && !seen[word] {
			seen[word] = true
			ids = append(ids, word)
		}
	}
	return ids, nil
}

// malformedID tells if the word is not a test record's ID, but its longest prefix that is one goes on with a
// hyphen or a lower-case letter, such as ST-FIX-INV01-T-holds or ST-FIX-INV01-Tx. A word such as
// ST-STD-TRUST-R04, the ID of a record that is not a test, is not malformed: its longest such prefix, ST-STD-T,
// goes on with an upper-case letter.
func malformedID(word string) bool {
	if !strings.HasPrefix(word, "ST-") || isTestRecordID(word) {
		return false
	}
	for end := len(word) - 1; end > 0; end-- {
		if isTestRecordID(word[:end]) {
			next := word[end]
			return next == '-' || next >= 'a' && next <= 'z'
		}
	}
	return false
}

// isTestRecordID tells if the word is a test record's ID: ST, one or more parts of upper-case letters and
// digits, and a last part of T and digits, joined by hyphens.
func isTestRecordID(word string) bool {
	parts := strings.Split(word, "-")
	if len(parts) < 3 || parts[0] != "ST" {
		return false
	}
	for _, part := range parts[1 : len(parts)-1] {
		if part == "" || strings.IndexFunc(part, func(r rune) bool { return !(r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') }) >= 0 {
			return false
		}
	}
	last := parts[len(parts)-1]
	if !strings.HasPrefix(last, "T") {
		return false
	}
	return strings.IndexFunc(last[1:], func(r rune) bool { return r < '0' || r > '9' }) < 0
}
