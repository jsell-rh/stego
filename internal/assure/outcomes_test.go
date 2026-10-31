package assure

import (
	"strings"
	"testing"
	"time"
)

// emptyPython is the document of a Python run with no test.
const emptyPython = `{"format": 1, "started": "2026-10-29T14:10:00Z", "finished": "2026-10-29T14:10:01Z", "tests": []}`

// goOutput is go test -json output, as cmd/test2json writes it, with tests that carry IDs and tests that do not.
const goOutput = `{"Time":"2026-10-29T14:02:11Z","Action":"start","Package":"example.com/fix"}
{"Time":"2026-10-29T14:02:12Z","Action":"run","Package":"example.com/fix","Test":"TestFixture"}
{"Time":"2026-10-29T14:02:12Z","Action":"output","Package":"example.com/fix","Test":"TestFixture","Output":"=== RUN   TestFixture\n"}
{"Time":"2026-10-29T14:02:12Z","Action":"run","Package":"example.com/fix","Test":"TestFixture/ST-FIX-SP03-T_and_ST-FIX-SP02-T_hold"}
{"Time":"2026-10-29T14:02:12Z","Action":"output","Package":"example.com/fix","Test":"TestFixture/ST-FIX-SP03-T_and_ST-FIX-SP02-T_hold","Output":"=== RUN   TestFixture/ST-FIX-SP03-T_and_ST-FIX-SP02-T_hold\n"}
{"Time":"2026-10-29T14:02:12Z","Action":"output","Package":"example.com/fix","Test":"TestFixture/ST-FIX-SP03-T_and_ST-FIX-SP02-T_hold","Output":"    fix_test.go:12: want 1, got 2\n"}
{"Time":"2026-10-29T14:02:12Z","Action":"output","Package":"example.com/fix","Test":"TestFixture/ST-FIX-SP03-T_and_ST-FIX-SP02-T_hold","Output":"--- FAIL: TestFixture/ST-FIX-SP03-T_and_ST-FIX-SP02-T_hold (0.10s)\n"}
{"Time":"2026-10-29T14:02:12Z","Action":"fail","Package":"example.com/fix","Test":"TestFixture/ST-FIX-SP03-T_and_ST-FIX-SP02-T_hold","Elapsed":0.1}
{"Time":"2026-10-29T14:02:13Z","Action":"run","Package":"example.com/fix","Test":"TestFixture/ST-FIX-SP01-T_holds_a/b"}
{"Time":"2026-10-29T14:02:13Z","Action":"pass","Package":"example.com/fix","Test":"TestFixture/ST-FIX-SP01-T_holds_a/b","Elapsed":0.25}
{"Time":"2026-10-29T14:02:13Z","Action":"run","Package":"example.com/fix","Test":"TestFixture/ST-FIX-INV01-T_holds"}
{"Time":"2026-10-29T14:02:13Z","Action":"run","Package":"example.com/fix","Test":"TestFixture/ST-FIX-INV01-T_holds/case_one"}
{"Time":"2026-10-29T14:02:13Z","Action":"output","Package":"example.com/fix","Test":"TestFixture/ST-FIX-INV01-T_holds/case_one","Output":"    fix_test.go:30: no database\n"}
{"Time":"2026-10-29T14:02:13Z","Action":"skip","Package":"example.com/fix","Test":"TestFixture/ST-FIX-INV01-T_holds/case_one","Elapsed":0}
{"Time":"2026-10-29T14:02:13Z","Action":"pass","Package":"example.com/fix","Test":"TestFixture/ST-FIX-INV01-T_holds","Elapsed":0.01}
{"Time":"2026-10-29T14:02:14Z","Action":"run","Package":"example.com/fix","Test":"TestFixture/a_case_with_no_ID"}
{"Time":"2026-10-29T14:02:14Z","Action":"fail","Package":"example.com/fix","Test":"TestFixture/a_case_with_no_ID","Elapsed":0}
{"Time":"2026-10-29T14:02:14Z","Action":"run","Package":"example.com/fix","Test":"TestFixture/LAST-FIX-SP04-T_and_ST-STD-TRUST-R04_are_not_test_IDs"}
{"Time":"2026-10-29T14:02:14Z","Action":"fail","Package":"example.com/fix","Test":"TestFixture/LAST-FIX-SP04-T_and_ST-STD-TRUST-R04_are_not_test_IDs","Elapsed":0}
{"Time":"2026-10-29T14:02:15Z","Action":"fail","Package":"example.com/fix","Test":"TestFixture","Elapsed":0.4}
{"Time":"2026-10-29T14:02:19Z","Action":"fail","Package":"example.com/fix","Elapsed":8}
`

// pythonOutput is the document scripts/run-python-tests.py writes, with subtests that carry IDs, not in ID order.
const pythonOutput = `{"format": 1, "started": "2026-10-29T14:01:00Z", "finished": "2026-10-29T14:01:02Z", "tests": [
 {"test": "test_fix.Fix.test_cases [ST-FIX-UC01-R02-T01 a case is skipped]", "name": "[ST-FIX-UC01-R02-T01 a case is skipped]", "result": "skipped", "elapsed": null, "detail": "no network"},
 {"test": "test_fix.Fix.test_cases [ST-FIX-UC01-R01-T01 a case holds]", "name": "[ST-FIX-UC01-R01-T01 a case holds]", "result": "passed", "elapsed": null, "detail": ""},
 {"test": "test_fix.Fix.test_cases", "name": "test_cases", "result": "failed", "elapsed": 0.5, "detail": "a subtest failed"},
 {"test": "test_fix.Fix.test_ST_FIX_UC01_R03_T01", "name": "test_ST_FIX_UC01_R03_T01", "result": "passed", "elapsed": 0.2, "detail": ""}
]}`

func TestReadOutcomes(t *testing.T) {
	t.Run("ST-ASR-DS01-T01 Go test outcomes are keyed by the IDs in each test's own name and ordered by ID", func(t *testing.T) {
		suites, err := ReadOutcomes(strings.NewReader(goOutput), strings.NewReader(emptyPython))
		if err != nil {
			t.Fatal(err)
		}
		want := []Outcome{
			{TestRecord: "ST-FIX-INV01-T", Result: Passed, Duration: 0.01, Timed: true},
			{TestRecord: "ST-FIX-SP01-T", Result: Passed, Duration: 0.25, Timed: true},
			{TestRecord: "ST-FIX-SP02-T", Result: Failed, Duration: 0.1, Timed: true, Detail: "fix_test.go:12: want 1, got 2"},
			{TestRecord: "ST-FIX-SP03-T", Result: Failed, Duration: 0.1, Timed: true, Detail: "fix_test.go:12: want 1, got 2"},
		}
		sameOutcomes(t, suites.Outcomes, want)
		if got := suites.StartedAt.Format(time.RFC3339); got != "2026-10-29T14:02:11Z" {
			t.Fatalf("started at %s, want the first event's time", got)
		}
		if got := suites.FinishedAt.Format(time.RFC3339); got != "2026-10-29T14:10:01Z" {
			t.Fatalf("finished at %s, want the last time in either output", got)
		}
		for _, word := range []string{"ST-FIX-INV01-T-holds", "ST-FIX-SP05-Tx"} {
			malformed := strings.Replace(goOutput, "ST-STD-TRUST-R04", word, -1)
			_, err := ReadOutcomes(strings.NewReader(malformed), strings.NewReader(emptyPython))
			if err == nil || !strings.Contains(err.Error(), `"`+word+`"`) {
				t.Fatalf("a test whose name holds %s is read, or the refusal does not name it: %v", word, err)
			}
			python := strings.Replace(pythonOutput, "ST-FIX-UC01-R01-T01 a case", word+" a case", -1)
			if _, err := ReadOutcomes(strings.NewReader(""), strings.NewReader(python)); err == nil || !strings.Contains(err.Error(), `"`+word+`"`) {
				t.Fatalf("a Python subtest whose name holds %s is read, or the refusal does not name it: %v", word, err)
			}
		}
	})

	t.Run("ST-ASR-DS01-T02 Python test outcomes are keyed by the IDs in each subtest's description", func(t *testing.T) {
		suites, err := ReadOutcomes(strings.NewReader(""), strings.NewReader(pythonOutput))
		if err != nil {
			t.Fatal(err)
		}
		want := []Outcome{
			{TestRecord: "ST-FIX-UC01-R01-T01", Result: Passed},
			{TestRecord: "ST-FIX-UC01-R02-T01", Result: Skipped, Detail: "no network"},
		}
		sameOutcomes(t, suites.Outcomes, want)
		if got := suites.StartedAt.Format(time.RFC3339); got != "2026-10-29T14:01:00Z" {
			t.Fatalf("started at %s", got)
		}
		for name, document := range map[string]string{
			"a result that is not passed, failed or skipped": strings.Replace(pythonOutput, `"result": "skipped"`, `"result": "error"`, 1),
			"another format": strings.Replace(pythonOutput, `"format": 1`, `"format": 2`, 1),
			"no document":    "",
		} {
			if _, err := ReadOutcomes(strings.NewReader(""), strings.NewReader(document)); err == nil {
				t.Fatalf("%s is read", name)
			}
		}
		if _, err := ReadOutcomes(strings.NewReader("PASS\nok example.com/fix 0.1s\n"), strings.NewReader(pythonOutput)); err == nil {
			t.Fatal("go test output that is not JSON is read")
		}
	})
}

func sameOutcomes(t *testing.T, got, want []Outcome) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d outcomes, want %d:\n got %+v\nwant %+v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("outcome %d:\n got %+v\nwant %+v", i, got[i], want[i])
		}
	}
}
