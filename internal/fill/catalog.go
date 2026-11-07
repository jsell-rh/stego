package fill

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jsell-rh/stego/internal/compiler"
	"github.com/jsell-rh/stego/internal/types"
)

// CatalogEntry is one fill of the project that implements a slot, with its
// collection and its qualification standing; or one fill folder whose
// manifest cannot be read, with its fault (ST-EXT-RM01).
type CatalogEntry struct {
	Fill       string
	Collection string
	Standing   Standing
	Reason     string
	Unreadable string
}

// ResolveSlot finds the one slot of the active component set that a slot
// name names, with the component that owns it when it is given. It refuses a
// name that no component of the active set declares, and names the slots the
// set declares; a name that more than one component declares when no
// component is given, and names each of them; and a component that the
// active set does not hold or that does not declare the slot.
func ResolveSlot(check compiler.FillCheck, slot, component string) (types.SlotRef, error) {
	var owners []types.SlotRef
	components := map[string]bool{}
	names := map[string]bool{}
	for _, s := range check.Slots {
		components[s.Ref.Component] = true
		names[s.Ref.Slot] = true
		if s.Ref.Slot == slot {
			owners = append(owners, s.Ref)
		}
	}
	if component != "" {
		if !components[component] {
			return types.SlotRef{}, fmt.Errorf("component %q is not in the active component set", component)
		}
		for _, owner := range owners {
			if owner.Component == component {
				return owner, nil
			}
		}
		return types.SlotRef{}, fmt.Errorf("component %q of the active component set does not declare the slot %q", component, slot)
	}
	switch len(owners) {
	case 0:
		return types.SlotRef{}, fmt.Errorf("slot %q is not declared by any component of the active component set; it declares the slots: %s", slot, strings.Join(sortedKeys(names), ", "))
	case 1:
		return owners[0], nil
	}
	var owning []string
	for _, owner := range owners {
		owning = append(owning, owner.Component)
	}
	sort.Strings(owning)
	return types.SlotRef{}, fmt.Errorf("slot %q is declared by more than one component of the active component set: %s; give the component with --component", slot, strings.Join(owning, ", "))
}

// ListCatalog answers ST-EXT-QRY01: each fill of the project that implements
// the slot, with its collection and its standing, and each fill folder whose
// manifest cannot be read, with its fault, in the order of the fill names.
// It writes nothing.
func ListCatalog(check compiler.FillCheck, slot types.SlotRef) ([]CatalogEntry, error) {
	entries, err := projectCatalog(check, slot)
	if err != nil {
		return nil, err
	}
	return NewCatalogReader(entries).Entries(), nil
}

// projectCatalog builds the catalog of one slot from the fill folders of the
// project, their slot contracts and SIGNERS.
func projectCatalog(check compiler.FillCheck, slot types.SlotRef) ([]CatalogEntry, error) {
	signers, err := ReadSigners(check.ProjectDir)
	if err != nil {
		return nil, err
	}
	names, err := fillFolders(check.ProjectDir)
	if err != nil {
		return nil, err
	}
	var entries []CatalogEntry
	for _, name := range names {
		dir := filepath.Join(check.ProjectDir, "fills", name)
		f, err := ParseFill(filepath.Join(dir, ManifestFile))
		if err != nil {
			entries = append(entries, CatalogEntry{Fill: name, Unreadable: strings.Join(strings.Fields(err.Error()), " ")})
			continue
		}
		if f.Implements != slot.String() {
			continue
		}
		current, err := CurrentState(dir, f, check)
		if err != nil {
			entries = append(entries, CatalogEntry{Fill: name, Collection: f.Collection, Unreadable: err.Error()})
			continue
		}
		standing, reason := assess(f, current, signers)
		entries = append(entries, CatalogEntry{Fill: name, Collection: f.Collection, Standing: standing, Reason: reason})
	}
	return entries, nil
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
