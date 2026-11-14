package assure

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// prefixPass is the form of a match of a pass line by a name prefix: the text "--- PASS: " and a test name, then
// an ID that is added to it, then "_", as in "--- PASS: TestCrud/"+id+"_". A pass of a subtest under the subtest of
// the ID also matches it (ST-STD-CODE-R07).
var prefixPass = regexp.MustCompile("--- PASS: [^\"`\\n]*[\"`]\\s*\\+\\s*[A-Za-z_][A-Za-z0-9_.]*\\s*\\+\\s*[\"`]_")

func TestNoPrefixPassMatch(t *testing.T) {
	t.Run("ST-STD-CODE-R07-T09 No test file of the tree matches the pass line of a record's subtest by a name prefix", func(t *testing.T) {
		for _, line := range []string{`"--- PASS: TestCrud/"+id+"_"`, "`--- PASS: Test[A-Za-z]+/` + id + `_`"} {
			if !prefixPass.MatchString(line) {
				t.Fatalf("the check does not find the prefix match %s", line)
			}
		}
		if prefixPass.MatchString("`(?m)^    --- PASS: ` + parent + `/` + regexp.QuoteMeta(id) + `_`") {
			t.Fatal("the check finds the match of ownPass, which is not by a prefix")
		}
		root := filepath.Join("..", "..")
		list, err := exec.Command("git", "-C", root, "ls-files", "*_test.go").Output()
		if err != nil {
			t.Fatal(err)
		}
		files := strings.Fields(string(list))
		if len(files) < 100 {
			t.Fatalf("git lists %d test files, want the tree's", len(files))
		}
		for _, name := range files {
			if strings.HasPrefix(name, "examples/") || name == "internal/assure/subtest_pass_test.go" {
				continue
			}
			data, err := os.ReadFile(filepath.Join(root, name))
			if err != nil {
				t.Fatal(err)
			}
			for i, line := range strings.Split(string(data), "\n") {
				if prefixPass.MatchString(line) {
					t.Errorf("%s:%d matches the pass line of a record's subtest by a name prefix; use ownPass: %s", name, i+1, strings.TrimSpace(line))
				}
			}
		}
	})
}
