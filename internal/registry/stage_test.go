package registry

import (
	"strings"
	"testing"
	"time"

	"github.com/jsell-rh/stego/internal/types"
)

func TestComponentPastItsSunset(t *testing.T) {
	t.Run("ST-SUP-SP04-T A component is past its sunset only when it is alpha or beta and its sunset date is earlier than today", func(t *testing.T) {
		today := time.Date(2026, 11, 4, 15, 30, 0, 0, time.UTC)
		for _, c := range []struct {
			stage  types.Stage
			sunset string
			past   bool
		}{
			{types.StageBeta, "2026-11-03", true},
			{types.StageBeta, "2026-11-04", false},
			{types.StageAlpha, "2026-11-05", false},
			{types.StageAlpha, "2026-10-01", true},
			{types.StageGA, "2026-10-01", false},
		} {
			component := &Component{Name: "exp-a", Stage: c.stage, Sunset: c.sunset}
			if got := PastSunset(component, today); got != c.past {
				t.Fatalf("%s component with the sunset %s: past %v, want %v", c.stage, c.sunset, got, c.past)
			}
		}
	})
}

func TestExperimentOwnsOnlyItsOwnPort(t *testing.T) {
	t.Run("ST-SUP-INV03-T A component below ga that provides the port of a ga component is refused", func(t *testing.T) {
		storeA := "kind: component\nname: store-a\nversion: 1.0.0\nstage: ga\nrequires: []\nprovides: [store-port]\nslots: []\n"
		betaStore := func(stage, port string) string {
			return "kind: component\nname: beta-store\nversion: 0.1.0\nstage: " + stage + "\nsunset: " + time.Now().AddDate(0, 0, 30).Format(time.DateOnly) + "\nrequires: []\nprovides: [" + port + "]\nslots: []\n"
		}
		for _, stage := range []string{"beta", "alpha"} {
			folder := writeRegistryFolder(t, map[string]string{
				"components/store-a/component.yaml":    storeA,
				"components/beta-store/component.yaml": betaStore(stage, "store-port"),
			})
			_, err := LoadSources(Source{Name: "common-registry", Dir: folder})
			if err == nil {
				t.Fatalf("beta-store at %s with store-port was accepted", stage)
			}
			for _, word := range []string{"beta-store", "(" + stage + ")", "store-port", "store-a"} {
				if !strings.Contains(err.Error(), word) {
					t.Fatalf("the refusal does not name %s: %v", word, err)
				}
			}
		}
		folder := writeRegistryFolder(t, map[string]string{
			"components/store-a/component.yaml":    storeA,
			"components/beta-store/component.yaml": betaStore("beta", "beta-store-port"),
		})
		if _, err := LoadSources(Source{Name: "common-registry", Dir: folder}); err != nil {
			t.Fatalf("beta-store with its own port was refused: %v", err)
		}
		common := Source{Name: "common-registry", Dir: writeRegistryFolder(t, map[string]string{"components/store-a/component.yaml": storeA})}
		local := Source{Name: "local-registry", Dir: writeRegistryFolder(t, map[string]string{"components/beta-store/component.yaml": betaStore("beta", "store-port")})}
		for _, order := range [][]Source{{common, local}, {local, common}} {
			_, err := LoadSources(order...)
			if err == nil || !strings.Contains(err.Error(), "store-port") || !strings.Contains(err.Error(), "beta-store") || !strings.Contains(err.Error(), "store-a") {
				t.Fatalf("the composition with %s first was not refused naming the port and both components: %v", order[0].Name, err)
			}
		}
	})
}
