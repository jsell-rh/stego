package main

import (
	"reflect"
	"testing"
)

func TestDeclaredModes(t *testing.T) {
	t.Run("ST-DECL-UC09-R01-T02 A declaration with no modes gets all three", func(t *testing.T) {
		document, run := validateJSON(t, td02Project(t, td02))
		if run.code != 0 || !document.Accepted {
			t.Fatalf("stego validate refused ST-DECL-TD02 (exit %d):\n%s\n%s", run.code, run.stdout, run.stderr)
		}
		for _, setting := range document.Settings {
			if setting.Path != "modes" {
				continue
			}
			if !reflect.DeepEqual(setting.Value, []any{"api", "worker", "controller"}) || !setting.Default {
				t.Fatalf("checking reports the modes %+v, want the default api, worker and controller", setting)
			}
			return
		}
		t.Fatalf("checking reports no modes:\n%s", run.stdout)
	})
}
