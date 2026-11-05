package types

import (
	"strings"
	"testing"
)

func TestMaturityStage(t *testing.T) {
	t.Run("ST-VO02-C01-T A maturity stage is exactly alpha, beta or ga", func(t *testing.T) {
		for _, value := range []string{"alpha", "beta", "ga"} {
			stage, err := ParseStage(value)
			if err != nil || string(stage) != value {
				t.Fatalf("%q was refused: %v", value, err)
			}
		}
		for _, value := range []string{"Alpha", "GA", "rc", "stable", "", " ga"} {
			_, err := ParseStage(value)
			if err == nil {
				t.Fatalf("%q was accepted", value)
			}
			for _, word := range []string{`"` + value + `"`, "alpha", "beta", "ga"} {
				if !strings.Contains(err.Error(), word) {
					t.Fatalf("the refusal of %q does not name %s: %v", value, word, err)
				}
			}
		}
	})
}
