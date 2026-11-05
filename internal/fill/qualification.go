package fill

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/jsell-rh/stego/internal/compiler"
	"github.com/jsell-rh/stego/internal/types"
)

// Standing is where one fill stands on its way to being qualified (ST-VO45).
type Standing string

// The qualification standings.
const (
	Qualified Standing = "qualified"
	Stale     Standing = "stale"
	Unsigned  Standing = "unsigned"
	Untested  Standing = "untested"
)

// ParseStanding reads a qualification standing, which is exactly one of
// qualified, stale, unsigned or untested (ST-VO45-C01).
func ParseStanding(text string) (Standing, error) {
	switch standing := Standing(text); standing {
	case Qualified, Stale, Unsigned, Untested:
		return standing, nil
	}
	return "", fmt.Errorf("%q is not a qualification standing: one of %s, %s, %s or %s", text, Qualified, Stale, Unsigned, Untested)
}

// Current is the state of a fill and of its slot contract now, which its
// evidence must name for the fill to be qualified.
type Current struct {
	FillDigest     string
	Files          map[string]string
	ContractDigest string
	Contract       types.SlotRef
	HasTests       bool
}

// IsQualified is the predicate of ST-EXT-SP01: the fill's evidence names its
// current fill digest and its slot contract's current digest, and a key that
// SIGNERS lists signed that evidence.
func IsQualified(f *Fill, fillDigest, contractDigest string, signers Signers) bool {
	standing, _ := assess(f, Current{FillDigest: fillDigest, ContractDigest: contractDigest, HasTests: true}, signers)
	return standing == Qualified
}

// FitsBinding is the predicate of ST-EXT-SP02: the fill implements the bound
// slot of the bound component and serves the bound collection.
func FitsBinding(f *Fill, slot types.SlotRef, collection string) bool {
	return misfit(f, []types.SlotRef{slot}, collection) == ""
}

// misfit names why a fill does not fit a binding of one of the slots for the
// collection, or gives "" when it fits.
func misfit(f *Fill, slots []types.SlotRef, collection string) string {
	ref, err := types.ParseSlotRef(f.Implements)
	if err != nil || !slices.Contains(slots, ref) {
		names := make([]string, len(slots))
		for i, slot := range slots {
			names[i] = slot.String()
		}
		return fmt.Sprintf("fill %q implements %s, not the bound slot %s, for the collection %s", f.Name, f.Implements, strings.Join(names, " or "), collection)
	}
	if f.Collection != collection {
		return fmt.Sprintf("fill %q serves the collection %s, not the bound collection %s, at the slot %s", f.Name, f.Collection, collection, ref)
	}
	return ""
}

// assess gives the standing of a fill and, when it is not qualified, why.
func assess(f *Fill, current Current, signers Signers) (Standing, string) {
	if !current.HasTests {
		return Untested, "it has no test file"
	}
	if f.Evidence == nil {
		return Untested, "it has no evidence"
	}
	var changed []string
	if f.Evidence.FillDigest != current.FillDigest {
		changed = append(changed, changedFiles(f.Evidence.Files, current.Files)...)
		if len(changed) == 0 {
			changed = append(changed, "the fill")
		}
	}
	if f.Evidence.ContractDigest != current.ContractDigest {
		contract := current.Contract.String()
		if current.Contract == (types.SlotRef{}) {
			contract = f.Implements
		}
		changed = append(changed, "the contract "+contract)
	}
	if len(changed) > 0 {
		return Stale, strings.Join(changed, ", ") + " changed after its evidence"
	}
	if f.SignOff == nil {
		return Unsigned, "it has no sign-off"
	}
	digest, err := EvidenceDigest(f)
	if err != nil {
		return Unsigned, err.Error()
	}
	if _, err := VerifySignOff(f.SignOff, digest, signers); err != nil {
		switch {
		case errors.Is(err, ErrUnlistedKey):
			return Unsigned, "SIGNERS does not list its key"
		default:
			return Unsigned, "its sign-off does not sign its evidence"
		}
	}
	return Qualified, ""
}

// changedFiles names the files that differ between the digests that the
// evidence holds and the digests now, in the order of their names.
func changedFiles(before, now map[string]string) []string {
	var names []string
	for name, digest := range now {
		if before[name] != digest {
			names = append(names, "the file "+name)
		}
	}
	for name := range before {
		if _, held := now[name]; !held {
			names = append(names, "the file "+name)
		}
	}
	sort.Strings(names)
	return names
}

// Qualification is one fill that the declaration binds, with its standing,
// whether it fits each of its bindings, and why it is not qualified or does
// not fit (ST-EXT-RM02).
type Qualification struct {
	Fill     string
	Standing Standing
	Fits     bool
	Reasons  []string
}

// ListQualification answers ST-EXT-QRY02: each fill that the declaration
// binds, with its standing and its fit, in the order of the fill names. It
// writes nothing.
func ListQualification(check compiler.FillCheck) ([]Qualification, error) {
	rows, err := projectQualification(check)
	if err != nil {
		return nil, err
	}
	return NewQualificationReader(rows).Rows(), nil
}

// projectQualification builds the qualification of each bound fill from its
// manifest, its files, its slot contract and SIGNERS.
func projectQualification(check compiler.FillCheck) ([]Qualification, error) {
	signers, err := ReadSigners(check.ProjectDir)
	if err != nil {
		return nil, err
	}
	bindings := map[string][]compiler.FillBinding{}
	var names []string
	for _, binding := range check.Bindings {
		if _, seen := bindings[binding.Fill]; !seen {
			names = append(names, binding.Fill)
		}
		bindings[binding.Fill] = append(bindings[binding.Fill], binding)
	}
	sort.Strings(names)
	var rows []Qualification
	for _, name := range names {
		row := Qualification{Fill: name, Fits: true}
		dir := filepath.Join(check.ProjectDir, "fills", name)
		f, err := ReadManifest(filepath.Join(dir, ManifestFile), check)
		if err != nil {
			row.Standing, row.Fits = Untested, false
			row.Reasons = append(row.Reasons, err.Error())
			rows = append(rows, row)
			continue
		}
		for _, binding := range bindings[name] {
			if reason := misfit(f, slotsNamed(check, binding.Slot), binding.Collection); reason != "" {
				row.Fits = false
				row.Reasons = append(row.Reasons, reason)
			}
		}
		current, err := CurrentState(dir, f, check)
		if err != nil {
			return nil, err
		}
		standing, reason := assess(f, current, signers)
		row.Standing = standing
		if reason != "" {
			row.Reasons = append(row.Reasons, reason)
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// slotsNamed gives each active slot of the name.
func slotsNamed(check compiler.FillCheck, name string) []types.SlotRef {
	var refs []types.SlotRef
	for _, slot := range check.Slots {
		if slot.Ref.Slot == name {
			refs = append(refs, slot.Ref)
		}
	}
	return refs
}

// CurrentState reads the fill's files and its slot contract as they are now.
func CurrentState(dir string, f *Fill, check compiler.FillCheck) (Current, error) {
	files, err := FileDigests(dir)
	if err != nil {
		return Current{}, err
	}
	current := Current{FillDigest: combine(files), Files: files}
	for name := range files {
		if strings.HasSuffix(name, "_test.go") {
			current.HasTests = true
		}
	}
	ref, err := types.ParseSlotRef(f.Implements)
	if err != nil {
		return Current{}, err
	}
	slot, ok := findSlot(check, ref)
	if !ok {
		return Current{}, fmt.Errorf("fill %q: implements: %s is not a slot of the active component set", f.Name, f.Implements)
	}
	current.Contract = ref
	current.ContractDigest, err = ContractDigest(check.Contracts, slot.Contract)
	if err != nil {
		return Current{}, err
	}
	return current, nil
}

// findSlot finds the active slot of a reference.
func findSlot(check compiler.FillCheck, ref types.SlotRef) (compiler.FillSlot, bool) {
	for _, slot := range check.Slots {
		if slot.Ref == ref {
			return slot, true
		}
	}
	return compiler.FillSlot{}, false
}

// ReadManifest reads a fill manifest and checks it against the declaration
// and the active registry (ST-EXT-INV01): it names exactly one slot of a
// component of the active set, and one collection of the declaration. A
// refusal names the fill and the field.
func ReadManifest(path string, check compiler.FillCheck) (*Fill, error) {
	f, err := ParseFill(path)
	if err != nil {
		return nil, err
	}
	name := f.Name
	if name == "" {
		name = filepath.Base(filepath.Dir(path))
	}
	if f.Implements == "" {
		return nil, fmt.Errorf("fill %q: implements: missing required field", name)
	}
	ref, err := types.ParseSlotRef(f.Implements)
	if err != nil {
		return nil, fmt.Errorf("fill %q: implements: %v", name, err)
	}
	if _, ok := findSlot(check, ref); !ok {
		return nil, fmt.Errorf("fill %q: implements: %s is not a slot of a component of the active set", name, ref)
	}
	if f.Collection == "" {
		return nil, fmt.Errorf("fill %q: collection: missing required field", name)
	}
	if !slices.Contains(check.Collections, f.Collection) {
		return nil, fmt.Errorf("fill %q: collection: %s is not a collection of the declaration", name, f.Collection)
	}
	return f, nil
}

// fillFolders lists the fill folders of the project, in the order of their
// names.
func fillFolders(projectDir string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(projectDir, "fills"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	return names, nil
}
