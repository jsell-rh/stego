package assure

import (
	"encoding/json"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// The proofs of the pattern ST-PAT-GO-CODE in one domain: its rules R01 (each hand-written technical record fills
// exactly one slot), R02 (each fill serves an essential record) and R03 (the fills meet the code standard). Each
// domain's test makes a goCodeProof for its own ID and names each subtest with its test record, numbered by the
// domain's position.

// latheWorkspace is the part of lathe check --json that the pattern proofs read.
type latheWorkspace struct {
	Kinds []struct {
		Kind       string `json:"kind"`
		Stage      string `json:"stage"`
		Complexity string `json:"complexity"`
	} `json:"kinds"`
	Records []struct {
		ID     string         `json:"id"`
		Kind   string         `json:"kind"`
		Parent string         `json:"parent"`
		Domain string         `json:"domain"`
		Fields map[string]any `json:"fields"`
	} `json:"records"`
	Links []struct {
		Source   string `json:"source"`
		Relation string `json:"relation"`
		Target   string `json:"target"`
	} `json:"links"`
}

// checkedWorkspace runs lathe check --json on the spec of the repository and gives its product's workspace.
func checkedWorkspace(t *testing.T) latheWorkspace {
	t.Helper()
	lathe := latheCLI(t)
	root, err := RepositoryRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(lathe, "check", "--json")
	command.Dir = root
	output, err := command.Output()
	if err != nil && len(output) == 0 {
		t.Fatalf("lathe check --json: %v", err)
	}
	var check struct {
		Products []struct {
			Workspace latheWorkspace `json:"workspace"`
		} `json:"products"`
	}
	if err := json.Unmarshal(output, &check); err != nil || len(check.Products) == 0 {
		t.Fatalf("the output is not lathe check --json: %v", err)
	}
	return check.Products[0].Workspace
}

// domainFills gives each hand-written technical record of the domain with the slots it fills: the records of the
// technical stage that nothing provides, and that no tooling record generates.
func domainFills(workspace latheWorkspace, domain string) (map[string][]string, map[string]bool) {
	technical := map[string]bool{}
	for _, kind := range workspace.Kinds {
		if kind.Stage == "technical-specification" {
			technical[kind.Kind] = true
		}
	}
	provided := map[string]bool{}
	fills := map[string][]string{}
	for _, link := range workspace.Links {
		switch link.Relation {
		case "provided_by", "generates":
			provided[link.Source] = true
		case "fills":
			fills[link.Source] = append(fills[link.Source], link.Target)
		}
	}
	records := map[string][]string{}
	for _, record := range workspace.Records {
		if record.Domain == domain && technical[record.Kind] && !provided[record.ID] {
			records[record.ID] = fills[record.ID]
		}
	}
	return records, technical
}

// goCodeProof holds what the three proofs of one domain read.
type goCodeProof struct {
	domain    string
	workspace latheWorkspace
	records   map[string][]string
}

func newGoCodeProof(t *testing.T, domain string) goCodeProof {
	t.Helper()
	workspace := checkedWorkspace(t)
	records, _ := domainFills(workspace, domain)
	if len(records) == 0 {
		t.Fatalf("the spec names no hand-written technical record of %s", domain)
	}
	return goCodeProof{domain: domain, workspace: workspace, records: records}
}

// oneSlotEach proves ST-PAT-GO-CODE-R01 in the domain.
func (p goCodeProof) oneSlotEach(t *testing.T) {
	records := p.records
	{
		for id, slots := range records {
			if len(slots) != 1 {
				t.Errorf("%s fills %d slots of ST-PAT-GO-CODE, not one: %v", id, len(slots), slots)
			}
			for _, slot := range slots {
				if !strings.HasPrefix(slot, "ST-PAT-GO-CODE-SL") {
					t.Errorf("%s fills %s, which is not a slot of ST-PAT-GO-CODE", id, slot)
				}
			}
		}
	}
}

// servesAnEssentialRecord proves ST-PAT-GO-CODE-R02 in the domain.
func (p goCodeProof) servesAnEssentialRecord(t *testing.T) {
	workspace, records := p.workspace, p.records
	{
		essential := map[string]bool{}
		for _, kind := range workspace.Kinds {
			if kind.Complexity == "essential" {
				essential[kind.Kind] = true
			}
		}
		byID := map[string]int{}
		for i, record := range workspace.Records {
			byID[record.ID] = i
		}
		isEssential := func(id string) bool {
			i, held := byID[id]
			return held && essential[workspace.Records[i].Kind]
		}
		serves := map[string][]string{}
		for _, link := range workspace.Links {
			if link.Relation == "implements" || link.Relation == "builds_on" {
				serves[link.Source] = append(serves[link.Source], link.Target)
			}
		}
		for id := range records {
			served := slices.ContainsFunc(serves[id], isEssential)
			for parent := workspace.Records[byID[id]].Parent; !served && parent != ""; {
				served = isEssential(parent)
				i, held := byID[parent]
				if !held {
					break
				}
				parent = workspace.Records[i].Parent
			}
			if !served {
				t.Errorf("%s implements, builds or sits under no essential record", id)
			}
		}
	}
}

// meetsTheCodeStandard proves ST-PAT-GO-CODE-R03 in the domain, against the list of known faults and its ratchet.
func (p goCodeProof) meetsTheCodeStandard(t *testing.T) {
	domain, records := p.domain, p.records
	{
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
		// The files that the domain's fills name, with their helper files, and their packages.
		files := map[string]bool{}
		for _, record := range spec.Technical {
			if _, filled := records[record.ID]; filled {
				files[record.File] = true
				for _, helper := range record.Helpers {
					files[helper] = true
				}
			}
		}
		inScope := func(fault CodeFault) bool {
			if files[fault.Path] {
				return true
			}
			for file := range files {
				if strings.HasPrefix(file, fault.Path+"/") && !strings.Contains(strings.TrimPrefix(file, fault.Path+"/"), "/") {
					return true
				}
			}
			return false
		}
		all, err := CodeFaults(spec, tree)
		if err != nil {
			t.Fatal(err)
		}
		var list TechnicalRecord
		for _, record := range spec.Technical {
			if record.ID == knownCodeFaultsRecord {
				list = record
			}
		}
		data, held := tree.File(list.File)
		if !held {
			t.Fatalf("the spec at HEAD has no list of known faults %s", knownCodeFaultsRecord)
		}
		known, err := readKnownCodeFaults(data)
		if err != nil {
			t.Fatal(err)
		}
		var got, listed []CodeFault
		for _, fault := range all {
			if inScope(fault) {
				got = append(got, fault)
			}
		}
		for _, fault := range known {
			if inScope(fault) {
				listed = append(listed, fault)
			}
		}
		if added := difference(got, listed); len(added) > 0 {
			t.Errorf("%d faults in the files of %s's fills that the list of known faults does not hold:\n%s", len(added), domain, faultLines(added))
		}
		if gone := difference(listed, got); len(gone) > 0 {
			t.Errorf("%d listed faults of %s's fills are gone:\n%s", len(gone), domain, faultLines(gone))
		}
		base, why, skip := ratchetBase(t, root)
		if skip || base == "" {
			t.Log("the ratchet against the base is not checked: " + why)
			return
		}
		baseKeys, ratchet := baseKnownCodeFaults(t, root, base)
		if !ratchet {
			return
		}
		for _, fault := range listed {
			if !baseKeys[[2]string{fault.Rule, fault.Key}] {
				t.Errorf("the fault %s %s of %s's fills is not in the list at the base commit %s", fault.Rule, fault.Key, domain, base)
			}
		}
	}
}

func TestDistributingFollowsTheGoCodePattern(t *testing.T) {
	proof := newGoCodeProof(t, "ST-DIS")
	t.Run("ST-PAT-GO-CODE-R01-T04 Distributing's hand-written technical records each fill exactly one slot", proof.oneSlotEach)
	t.Run("ST-PAT-GO-CODE-R02-T04 Distributing's fills each serve an essential record", proof.servesAnEssentialRecord)
	t.Run("ST-PAT-GO-CODE-R03-T04 Distributing's fills meet every rule of the code standard that applies to them", proof.meetsTheCodeStandard)
}
