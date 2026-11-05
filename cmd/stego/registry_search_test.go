package main

import (
	"strings"
	"testing"
)

// searchRows gives the component rows of stego registry search output: each
// row's fields, after the header line.
func searchRows(t *testing.T, output string) [][]string {
	t.Helper()
	lines := strings.Split(strings.TrimRight(output, "\n"), "\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "NAME") || !strings.Contains(lines[0], "STAGE") {
		t.Fatalf("search printed no header with a stage column:\n%s", output)
	}
	var rows [][]string
	for _, line := range lines[1:] {
		rows = append(rows, strings.Fields(line))
	}
	return rows
}

func TestRegistrySearchNamesStage(t *testing.T) {
	t.Run("ST-SUP-UC09-R01-T01 The search names every provider of a port with its stage", func(t *testing.T) {
		f := newSupplyingFixture(t)
		storeB := strings.ReplaceAll(commonRegistryFiles()["components/store-a/component.yaml"], "store-a", "store-b")
		storeB = strings.Replace(storeB, "internal/store", "internal/storeb", 1)
		commonSHA := commitRegistry(t, f.common, map[string]string{"components/store-b/component.yaml": storeB})
		registryProject(t, f.project, "", pinned(f.common, commonSHA), pinned(f.local, f.localSHA))
		rows := searchRows(t, f.runStegoIn(t, f.project, nil, "registry", "search", "--provides", "store-port").accepted(t))
		if len(rows) != 2 || rows[0][0] != "store-a" || rows[1][0] != "store-b" || rows[0][2] != "ga" || rows[1][2] != "ga" {
			t.Fatalf("search --provides store-port gave %v, want store-a and store-b in name order, each ga", rows)
		}
		rows = searchRows(t, f.runStegoIn(t, f.project, nil, "registry", "search", "--provides", "local-port").accepted(t))
		if len(rows) != 1 || rows[0][0] != "local-a" || rows[0][2] != "beta" || rows[0][3] != "local" {
			t.Fatalf("search --provides local-port gave %v, want local-a from the local registry at beta", rows)
		}
	})

	t.Run("ST-SUP-UC09-R01-T02 The search names every component that requires a port with its stage", func(t *testing.T) {
		f := newSupplyingFixture(t)
		apiB := "kind: component\nname: api-b\nversion: 0.1.0\nstage: beta\nsunset: " + daysFromToday(60) +
			"\noutput_namespace: internal/apib\nrequires: [store-port]\nprovides: []\nslots: []\n"
		localSHA := commitRegistry(t, f.local, map[string]string{"components/api-b/component.yaml": apiB})
		registryProject(t, f.project, "", pinned(f.common, f.commonSHA), pinned(f.local, localSHA))
		rows := searchRows(t, f.runStegoIn(t, f.project, nil, "registry", "search", "--requires", "store-port").accepted(t))
		if len(rows) != 2 || rows[0][0] != "api-a" || rows[0][2] != "ga" || rows[1][0] != "api-b" || rows[1][2] != "beta" {
			t.Fatalf("search --requires store-port gave %v, want api-a with ga and api-b with beta, in name order", rows)
		}
	})
}
