package registry

import "testing"

func TestEmittedFileIsStatic(t *testing.T) {
	t.Run("ST-SUP-SP06-T An emitted file is static only when two contrasting renders give the same bytes after the module path and the package name are normalised", func(t *testing.T) {
		modules := [2]string{"example.com/alpha/orders", "example.org/beta/tickets"}
		packages := [2]string{"orders", "tickets"}
		for _, pair := range []struct {
			name          string
			first, second string
			static        bool
		}{
			{"the same bytes", "package util\n\nfunc Max(a, b int) int { return max(a, b) }\n", "package util\n\nfunc Max(a, b int) int { return max(a, b) }\n", true},
			{"only the module path and the package name differ", "package orders\n\nimport \"example.com/alpha/orders/internal/store\"\n", "package tickets\n\nimport \"example.org/beta/tickets/internal/store\"\n", true},
			{"a line that a setting gives differs", "package orders\n\nconst retries = 3\n", "package tickets\n\nconst retries = 7\n", false},
			{"only a last newline differs", "package orders\n\nconst name = \"x\"\n", "package tickets\n\nconst name = \"x\"", false},
		} {
			if got := fileIsStatic([]byte(pair.first), []byte(pair.second), modules, packages); got != pair.static {
				t.Fatalf("%s: static %v, want %v", pair.name, got, pair.static)
			}
		}
	})
}
