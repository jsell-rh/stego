package registry

import (
	"strings"
	"testing"
)

func TestRegistryTrust(t *testing.T) {
	t.Run("ST-VO03-C01-T A registry trust is common or local, and no other value", func(t *testing.T) {
		for _, value := range []string{"common", "local"} {
			trust, err := ParseTrust(value)
			if err != nil || string(trust) != value {
				t.Fatalf("%q was refused: %v", value, err)
			}
		}
		for _, value := range []string{"Common", "global", "trusted", ""} {
			_, err := ParseTrust(value)
			if err == nil {
				t.Fatalf("%q was accepted", value)
			}
			for _, word := range []string{`"` + value + `"`, "common", "local"} {
				if !strings.Contains(err.Error(), word) {
					t.Fatalf("the refusal of %q does not name %s: %v", value, word, err)
				}
			}
		}
	})
}
