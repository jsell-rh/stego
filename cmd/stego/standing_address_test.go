package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStandingRefusesAnOptionAddress(t *testing.T) {
	t.Run("ST-DIS-UC05-R04-T02 A registry address that git would read as an option is refused, and no standing is read", func(t *testing.T) {
		marker := filepath.Join(t.TempDir(), "marker")
		records, err := gitHeadLifecycles("--upload-pack=touch "+marker, "")
		if err == nil || !strings.Contains(err.Error(), `starts with "-"`) || records != nil {
			t.Fatalf("got %v and %d records, want a refusal of the address", err, len(records))
		}
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("git ran the address as an option: %v", err)
		}
	})
}
