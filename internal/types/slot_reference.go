package types

import (
	"fmt"
	"strings"
)

// SlotRef is the one slot a fill implements (ST-VO46): a component of the
// registry, or a mixin that adds the slot, and the slot's name in it.
type SlotRef struct {
	Component string
	Slot      string
}

// ParseSlotRef reads a slot reference written as component.slot. It refuses
// any other form, such as a slot name alone or a list.
func ParseSlotRef(text string) (SlotRef, error) {
	component, slot, found := strings.Cut(text, ".")
	if !found || component == "" || slot == "" || strings.Contains(slot, ".") {
		return SlotRef{}, fmt.Errorf("%q is not a slot reference of the form component.slot", text)
	}
	return SlotRef{Component: component, Slot: slot}, nil
}

// String writes the slot reference as component.slot.
func (r SlotRef) String() string { return r.Component + "." + r.Slot }
