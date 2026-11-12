package fleet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeRecord writes a committed record into a new checkout and gives the checkout.
func writeRecord(t *testing.T, record string) string {
	t.Helper()
	checkout := t.TempDir()
	if err := os.MkdirAll(filepath.Join(checkout, ".stego"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, RecordPath), []byte(record), 0o644); err != nil {
		t.Fatal(err)
	}
	return checkout
}

func TestSweepReadsTheRuntimePin(t *testing.T) {
	t.Run("ST-DIS-UC02-R01-T03 The sweep reads records of format versions 2 and 3, and gives the runtime version that a format 3 record holds", func(t *testing.T) {
		digest := strings.Repeat("a", 64)
		body := "last_applied:\n    service_name: %s\n    components:\n        jwt-auth: {version: 2.4.0, sha: " + digest + "}\n"
		older := writeRecord(t, "format_version: 2\n"+strings.Replace(body, "%s", "alpha", 1))
		current := writeRecord(t, "format_version: 3\n"+strings.Replace(body, "%s", "bravo", 1)+
			"    archetype:\n        name: rest-crud\n        version: 3.1.0\n        runtime:\n            module: github.com/jsell-rh/stego/registry/runtime\n            version: v0.1.0\n            sum: h1:LBQv5u8LCsWvwtBuRBSnvN3h7IWSNQHlDW4Og0dSH2g=\n")
		pinless := writeRecord(t, "format_version: 3\n"+strings.Replace(body, "%s", "charlie", 1))
		future := writeRecord(t, "format_version: 6\n"+strings.Replace(body, "%s", "delta", 1))

		sweep := Project([]string{older, current, pinless, future})
		if len(sweep.Services) != 2 || sweep.Services[0].Name != "alpha" || sweep.Services[1].Name != "bravo" {
			t.Fatalf("services = %+v, want alpha and bravo", sweep.Services)
		}
		if sweep.Services[0].Runtime != nil {
			t.Fatalf("the format 2 record gave the runtime %+v", sweep.Services[0].Runtime)
		}
		want := Runtime{Module: "github.com/jsell-rh/stego/registry/runtime", Version: "v0.1.0", Sum: "h1:LBQv5u8LCsWvwtBuRBSnvN3h7IWSNQHlDW4Og0dSH2g="}
		if got := sweep.Services[1].Runtime; got == nil || *got != want {
			t.Fatalf("the format 3 record gave the runtime %+v, want %+v", got, want)
		}
		if len(sweep.Services[1].Pins) != 1 || sweep.Services[1].Pins[0].Digest != digest {
			t.Fatalf("pins = %+v", sweep.Services[1].Pins)
		}
		if len(sweep.Unknown) != 2 || sweep.Unknown[0].Path != pinless || !strings.Contains(sweep.Unknown[0].Reason, "runtime library pin") ||
			sweep.Unknown[1].Path != future || sweep.Unknown[1].FormatVersion != 6 {
			t.Fatalf("unknown = %+v, want the pinless record and format version 6", sweep.Unknown)
		}
	})
}
