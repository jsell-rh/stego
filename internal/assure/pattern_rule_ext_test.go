package assure

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
	"testing"
)

// The tests of the Go code pattern's rules in Extending (ST-EXT). They read
// STEGO's spec and tree at HEAD, as TestOwnTree does, so run them with
// -count=1 after a commit.

// extDomain is the prefix of the IDs of Extending's records.
const extDomain = "ST-EXT-"

// specRows reads every row of the spec's data files in the tree, by ID, with
// the table that holds it, and the link rows of each table by its name.
func specRows(t *testing.T, tree Tree) (map[string]string, map[string]map[string]any, map[string][]map[string]any) {
	t.Helper()
	tables := map[string]string{}
	rows := map[string]map[string]any{}
	links := map[string][]map[string]any{}
	for _, name := range tree.paths() {
		if path.Dir(name) != "data" || path.Ext(name) != ".jsonl" {
			continue
		}
		table := strings.TrimSuffix(path.Base(name), ".jsonl")
		data, _ := tree.File(name)
		scanner := bufio.NewScanner(bytes.NewReader(data))
		scanner.Buffer(make([]byte, 1<<20), 1<<24)
		for scanner.Scan() {
			var row map[string]any
			if json.Unmarshal(scanner.Bytes(), &row) != nil {
				continue
			}
			if id, ok := row["id"].(string); ok {
				tables[id] = table
				rows[id] = row
			} else {
				links[table] = append(links[table], row)
			}
		}
	}
	return tables, rows, links
}

// extTechnical gives the technical records of Extending that no tooling
// record generates and nothing provides, by ID.
func extTechnical(t *testing.T, spec Spec, links map[string][]map[string]any) map[string]TechnicalRecord {
	t.Helper()
	provided := map[string]bool{}
	for _, row := range links["provided_by"] {
		if source, ok := row["source_id"].(string); ok {
			provided[source] = true
		}
	}
	records := map[string]TechnicalRecord{}
	for _, record := range spec.Technical {
		if strings.HasPrefix(record.ID, extDomain) && record.Kind != "tooling" && !provided[record.ID] {
			records[record.ID] = record
		}
	}
	if len(records) == 0 {
		t.Fatal("the spec has no technical record of ST-EXT")
	}
	return records
}

func readHead(t *testing.T) (Spec, Tree) {
	t.Helper()
	root, err := RepositoryRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	spec, err := ReadCommittedSpec(root, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	tree, err := ReadCommittedTree(root, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return spec, tree
}

func TestExtendingFollowsTheGoCodePattern(t *testing.T) {
	spec, tree := readHead(t)
	tables, rows, links := specRows(t, tree)
	records := extTechnical(t, spec, links)
	technical := map[string]bool{}
	for _, record := range spec.Technical {
		technical[tables[record.ID]] = true
	}

	t.Run("ST-PAT-GO-CODE-R01-T06 Each hand-written technical record of Extending fills exactly one slot of the Go code pattern", func(t *testing.T) {
		fillings := map[string][]string{}
		for _, row := range links["fills"] {
			source, _ := row["source_id"].(string)
			target, _ := row["target_id"].(string)
			if strings.HasPrefix(target, "ST-PAT-GO-CODE-SL") {
				fillings[source] = append(fillings[source], target)
			}
		}
		var faults []string
		for id := range records {
			if n := len(fillings[id]); n != 1 {
				faults = append(faults, fmt.Sprintf("%s fills %d slots: %s", id, n, strings.Join(fillings[id], ", ")))
			}
		}
		sort.Strings(faults)
		if len(faults) > 0 {
			t.Fatalf("records of ST-EXT that do not fill exactly one slot of ST-PAT-GO-CODE:\n%s", strings.Join(faults, "\n"))
		}
	})

	t.Run("ST-PAT-GO-CODE-R02-T06 Each fill of the Go code pattern in Extending serves an essential record, and no Go file of Extending serves none", func(t *testing.T) {
		essential := func(id string) bool {
			table, ok := tables[id]
			return ok && !technical[table] && table != "domain" && table != "product"
		}
		// A record serves its parent, which its *_id field names, or, for a
		// record that sits only under its domain, what its links reach: a CLI
		// handler runs a CLI command, a repository persists an aggregate.
		reach := map[string][]string{}
		for _, table := range []string{"runs", "persists"} {
			for _, row := range links[table] {
				source, _ := row["source_id"].(string)
				target, _ := row["target_id"].(string)
				reach[source] = append(reach[source], target)
			}
		}
		var faults []string
		for id := range records {
			serves := false
			for field, value := range rows[id] {
				parent, ok := value.(string)
				if ok && field != "id" && field != "domain_id" && strings.HasSuffix(field, "_id") && essential(parent) {
					serves = true
				}
			}
			for _, target := range reach[id] {
				if essential(target) {
					serves = true
				}
			}
			if !serves {
				faults = append(faults, id+" serves no essential record")
			}
		}
		named := map[string]bool{}
		for _, record := range spec.Technical {
			named[record.File] = true
			for _, helper := range record.Helpers {
				named[helper] = true
			}
		}
		folders := map[string]bool{}
		for _, record := range records {
			if strings.HasSuffix(record.File, ".go") {
				folders[path.Dir(record.File)] = true
			}
		}
		for _, name := range tree.paths() {
			if folders[path.Dir(name)] && strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") && !named[name] {
				faults = append(faults, name+" is named by no technical record")
			}
		}
		sort.Strings(faults)
		if len(faults) > 0 {
			t.Fatalf("faults of ST-PAT-GO-CODE-R02 in ST-EXT:\n%s", strings.Join(faults, "\n"))
		}
	})

	t.Run("ST-PAT-GO-CODE-R03-T06 Each file of Extending meets every rule of the code standard that applies to its kind", func(t *testing.T) {
		files := map[string]bool{}
		keys := map[string]bool{}
		for id, record := range records {
			files[record.File] = true
			keys[id] = true
			for _, helper := range record.Helpers {
				files[helper] = true
			}
		}
		var list TechnicalRecord
		for _, record := range spec.Technical {
			if record.ID == knownCodeFaultsRecord {
				list = record
			}
		}
		data, held := tree.File(list.File)
		if !held {
			t.Fatalf("the tree holds no list of known faults %s", list.File)
		}
		known, err := readKnownCodeFaults(data)
		if err != nil {
			t.Fatal(err)
		}
		mine := func(fault CodeFault) bool { return files[fault.Path] || keys[fault.Key] }
		all, err := CodeFaults(spec, tree)
		if err != nil {
			t.Fatal(err)
		}
		var got, listed []CodeFault
		for _, fault := range all {
			if mine(fault) {
				got = append(got, fault)
			}
		}
		for _, fault := range known {
			if mine(fault) {
				listed = append(listed, fault)
			}
		}
		if added := difference(got, listed); len(added) > 0 {
			t.Fatalf("faults of ST-EXT files that the list of known faults does not hold:\n%s", faultLines(added))
		}
		if gone := difference(listed, got); len(gone) > 0 {
			t.Fatalf("listed faults of ST-EXT files that the tree no longer has:\n%s", faultLines(gone))
		}
	})
}
