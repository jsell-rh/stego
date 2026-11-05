package types

import (
	"strings"
	"testing"
)

func TestSlotReference(t *testing.T) {
	t.Run("ST-VO46-C01-T A slot reference is a component name, a period and a slot name, each not empty", func(t *testing.T) {
		for text, want := range map[string]SlotRef{
			"rest-api.before_create":            {Component: "rest-api", Slot: "before_create"},
			"event-publisher.on_entity_changed": {Component: "event-publisher", Slot: "on_entity_changed"},
		} {
			ref, err := ParseSlotRef(text)
			if err != nil || ref != want || ref.String() != text {
				t.Errorf("%q: got %+v, %v", text, ref, err)
			}
		}
		for _, text := range []string{"before_create", ".before_create", "rest-api.", ".", ""} {
			_, err := ParseSlotRef(text)
			if err == nil || !strings.Contains(err.Error(), "\""+text+"\"") {
				t.Errorf("%q: got %v, want a fault that names the reference", text, err)
			}
		}
	})
}
