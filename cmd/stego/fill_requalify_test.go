package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/fill"
)

// changeRegistryComment gives the project a copy of the built-in registry
// as its active registry, and changes one comment line of a file in it.
func changeRegistryComment(t *testing.T, p *fillProject, name string) {
	t.Helper()
	p.registry = copyFolder(t, p.keys.registry)
	path := p.registry + "/" + name
	data := string(readFile(t, path))
	index := strings.Index(data, "//")
	if index < 0 {
		data += "// A changed comment.\n"
	} else {
		data = data[:index+2] + " changed" + data[index+2:]
	}
	writeFile(t, p.registry, name, data, 0644)
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestFillRequalification(t *testing.T) {
	t.Run("ST-EXT-UC04-R01-T01 A change to a qualified fill's code makes it stale", func(t *testing.T) {
		p := newFillProject(t)
		p.bind(t, slotBinding{"qualified-fill", "before_create", "org-users"})
		expectStanding(t, p.must(t, "no-key", "fill", "status"), "qualified-fill", fill.Qualified)
		p.replace(t, "fills/qualified-fill/policy.go", "// Evaluate allows the request.", "// Evaluate allows the request now.")
		expectStanding(t, p.must(t, "no-key", "fill", "status"), "qualified-fill", fill.Stale)
	})

	t.Run("ST-EXT-UC04-R01-T02 A change to a qualified fill's test makes it stale", func(t *testing.T) {
		p := newFillProject(t)
		p.bind(t, slotBinding{"qualified-fill", "before_create", "org-users"})
		p.replace(t, "fills/qualified-fill/policy_test.go", "the policy denied", "the policy denied the request")
		expectStanding(t, p.must(t, "no-key", "fill", "status"), "qualified-fill", fill.Stale)
	})

	t.Run("ST-EXT-UC04-R01-T03 A change to the slot contract of a qualified fill makes it stale", func(t *testing.T) {
		p := newFillProject(t)
		p.bind(t, slotBinding{"qualified-fill", "before_create", "org-users"})
		changeRegistryComment(t, p, "components/rest-api/slots/before_create.proto")
		expectStanding(t, p.must(t, "no-key", "fill", "status"), "qualified-fill", fill.Stale)
	})

	t.Run("ST-EXT-UC04-R02-T01 fill status names each fill that is not qualified and why", func(t *testing.T) {
		p := newFillProject(t)
		p.bind(t,
			slotBinding{"no-tests-fill, untested-fill, unsigned-fill, unlisted-fill, stale-fill, qualified-fill", "before_create", "org-users"},
			slotBinding{"other-slot-fill", "before_delete", "org-users"})
		changeRegistryComment(t, p, "components/rest-api/slots/before_delete.proto")
		status := p.must(t, "no-key", "fill", "status")
		for name, reason := range map[string]string{
			"no-tests-fill":   "it has no test file",
			"untested-fill":   "it has no evidence",
			"unsigned-fill":   "it has no sign-off",
			"unlisted-fill":   "SIGNERS does not list its key",
			"stale-fill":      "the file policy.go changed after its evidence",
			"other-slot-fill": "the contract rest-api.before_delete changed after its evidence",
		} {
			line := statusOf(t, status, name)
			if fields := strings.Fields(line); len(fields) < 2 || fields[1] == string(fill.Qualified) || !strings.Contains(line, reason) {
				t.Errorf("fill status does not show %s as not qualified with the reason %q:\n%s", name, reason, line)
			}
		}
		line := expectStanding(t, status, "qualified-fill", fill.Qualified)
		if fields := strings.Fields(line); len(fields) != 3 {
			t.Fatalf("fill status gives qualified-fill a reason:\n%s", line)
		}
	})

	t.Run("ST-EXT-UC04-R03-T02 A new sign-off of the new evidence requalifies the fill", func(t *testing.T) {
		p := newFillProject(t)
		p.bind(t, slotBinding{"stale-fill", "before_create", "org-users"})
		p.must(t, "listed-ssh", "test")
		p.must(t, "listed-ssh", "fill", "sign", "stale-fill")
		expectStanding(t, p.must(t, "listed-ssh", "fill", "status"), "stale-fill", fill.Qualified)
		p.must(t, "listed-ssh", "plan")
	})

	t.Run("ST-EXT-UC04-R03-T03 stego fill sign refuses a stale fill before new evidence, and writes no sign-off", func(t *testing.T) {
		p := newFillProject(t)
		before := p.read(t, "fills/stale-fill/fill.yaml")
		stdout, stderr, err := p.stego(t, "listed-ssh", "fill", "sign", "stale-fill")
		if err == nil || !strings.Contains(stderr, `the evidence of fill "stale-fill" is stale`) {
			t.Fatalf("fill sign does not refuse stale-fill as stale: %v\n%s\n%s", err, stdout, stderr)
		}
		if !bytes.Equal(before, p.read(t, "fills/stale-fill/fill.yaml")) {
			t.Fatal("fill sign changed the manifest of stale-fill")
		}
	})
}
