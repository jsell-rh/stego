package types

import (
	"fmt"
	"slices"
)

// ValidateLiveUnique checks the explicit rules for live-row unique keys.
func ValidateLiveUnique(entities []Entity, collections []Collection) []error {
	var result []error
	for _, entity := range entities {
		fields := make(map[string]Field, len(entity.Fields))
		for _, field := range entity.Fields {
			fields[field.Name] = field
		}
		for _, field := range entity.Fields {
			if !field.UniqueWhenLive {
				continue
			}
			fail := func(reason string) {
				result = append(result, fmt.Errorf("entity %q field %q: unique_when_live %s", entity.Name, field.Name, reason))
			}
			if !field.Unique && len(field.UniqueComposite) == 0 {
				fail("requires unique or unique_composite")
			}
			if field.Unique && len(field.UniqueComposite) > 0 {
				fail("cannot combine unique and unique_composite")
			}
			if field.Computed {
				fail("cannot apply to a computed field")
			}
			if len(field.UniqueComposite) > 0 {
				if !slices.Contains(field.UniqueComposite, field.Name) {
					fail("requires this field in its composite key")
				}
				seen := make(map[string]bool)
				for _, name := range field.UniqueComposite {
					member, exists := fields[name]
					if seen[name] || !exists || !member.UniqueWhenLive || member.Computed || !slices.Equal(member.UniqueComposite, field.UniqueComposite) {
						fail("requires the same ordered composite key on each live member")
						break
					}
					seen[name] = true
				}
			}
		}
		for _, collection := range collections {
			if collection.Entity != entity.Name {
				continue
			}
			for _, name := range collection.UpsertKey {
				if fields[name].UniqueWhenLive {
					result = append(result, fmt.Errorf("collection %q: upsert_key cannot include unique_when_live field %q", collection.Name, name))
				}
			}
		}
	}
	return result
}
