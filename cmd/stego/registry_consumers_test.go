package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// consumersFixture is ST-SUP-TD03: the registry of ST-SUP-TD01 with the
// experiments beta-x and beta-old, and service checkouts that record which
// components they use.
type consumersFixture struct {
	*supplyingFixture
	checkouts map[string]string
}

// serviceCheckout commits a service checkout whose declaration names the
// service and whose generation record names each component at its version.
func serviceCheckout(t *testing.T, name string, components map[string]string) string {
	t.Helper()
	var state strings.Builder
	state.WriteString("format_version: 1\nlast_applied:\n  service_hash: fixture\n  components:\n")
	for component, version := range components {
		state.WriteString("    " + component + ":\n      version: " + version + "\n")
	}
	checkout, _ := registryRepository(t, name, map[string]string{
		"service.yaml":      "kind: service\nname: " + name + "\narchetype: svc-arch\nlanguage: go\n",
		".stego/state.yaml": state.String(),
	})
	return checkout
}

func newConsumersFixture(t *testing.T) *consumersFixture {
	t.Helper()
	f := &consumersFixture{supplyingFixture: newSupplyingFixture(t), checkouts: map[string]string{}}
	experiment := func(name, sunset string) string {
		return "kind: component\nname: " + name + "\nversion: 0.3.0\nstage: beta\nsunset: " + sunset +
			"\noutput_namespace: internal/" + name + "\nrequires: []\nprovides: [" + name + "-port]\nslots: []\n"
	}
	commonSHA := commitRegistry(t, f.common, map[string]string{
		"components/beta-x/component.yaml":   experiment("beta-x", daysFromToday(30)),
		"components/beta-old/component.yaml": experiment("beta-old", daysFromToday(-10)),
	})
	registryProject(t, f.project, "", pinned(f.common, commonSHA), pinned(f.local, f.localSHA))
	f.checkouts["svc-one"] = serviceCheckout(t, "svc-one", map[string]string{"beta-x": "0.2.0", "store-a": "1.0.0"})
	f.checkouts["svc-two"] = serviceCheckout(t, "svc-two", map[string]string{"beta-x": "0.3.0", "store-a": "1.0.0"})
	f.checkouts["svc-three"] = serviceCheckout(t, "svc-three", map[string]string{"store-a": "1.0.0"})
	f.checkouts["svc-four"] = serviceCheckout(t, "svc-four", map[string]string{"beta-old": "0.3.0"})
	return f
}

// paths gives the checkouts of the named services, in order.
func (f *consumersFixture) paths(names ...string) []string {
	var paths []string
	for _, name := range names {
		paths = append(paths, f.checkouts[name])
	}
	return paths
}

// checkoutState gives the git status and the bytes of every file of each
// checkout, so a test can see that a run changed none.
func checkoutState(t *testing.T, checkouts []string) string {
	t.Helper()
	var state strings.Builder
	for _, checkout := range checkouts {
		state.WriteString(git(t, checkout, "status", "--porcelain", "--ignored", "--untracked-files=all"))
		err := filepath.WalkDir(checkout, func(name string, entry os.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			if strings.Contains(name, string(filepath.Separator)+".git"+string(filepath.Separator)) && !strings.HasSuffix(name, "HEAD") {
				return nil
			}
			data, err := os.ReadFile(name)
			state.WriteString(name + "\n" + string(data))
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return state.String()
}

func TestRegistryConsumers(t *testing.T) {
	t.Parallel()
	t.Run("ST-SUP-UC05-R01-T01 stego registry consumers names every recorded service that uses a component, and no other", func(t *testing.T) {
		f := newConsumersFixture(t)
		checkouts := f.paths("svc-one", "svc-two", "svc-three", "svc-four")
		before := checkoutState(t, checkouts)
		output := f.runStegoIn(t, f.project, nil, append([]string{"registry", "consumers", "beta-x"}, checkouts...)...).accepted(t)
		for _, want := range []string{"svc-one 0.2.0", "svc-two 0.3.0"} {
			if !strings.Contains(output, want) {
				t.Fatalf("consumers does not name %s:\n%s", want, output)
			}
		}
		for _, other := range []string{"svc-three", "svc-four"} {
			if strings.Contains(output, other) {
				t.Fatalf("consumers names %s, which does not use beta-x:\n%s", other, output)
			}
		}
		if after := checkoutState(t, checkouts); after != before {
			t.Fatal("a file of a checkout changed")
		}
	})

	t.Run("ST-SUP-UC12-R02-T01 A missed sunset is reported for every service that still uses the component", func(t *testing.T) {
		f := newConsumersFixture(t)
		f.checkouts["svc-five"] = serviceCheckout(t, "svc-five", map[string]string{"beta-old": "0.3.0"})
		run := f.runStegoIn(t, f.project, nil, append([]string{"registry", "sunsets"}, f.paths("svc-one", "svc-two", "svc-three", "svc-four", "svc-five")...)...)
		if run.err == nil {
			t.Fatalf("sunsets exited 0 with a missed sunset:\n%s", run.stdout)
		}
		sunset := daysFromToday(-10)
		for _, service := range []string{"svc-four", "svc-five"} {
			if !strings.Contains(run.stdout, service+" uses beta-old") || !strings.Contains(run.stdout, sunset) {
				t.Fatalf("sunsets does not name %s with beta-old and its sunset %s:\n%s", service, sunset, run.stdout)
			}
		}
		for _, other := range []string{"svc-one", "svc-two", "svc-three"} {
			if strings.Contains(run.stdout, other) {
				t.Fatalf("sunsets names %s:\n%s", other, run.stdout)
			}
		}
	})

	t.Run("ST-SUP-UC12-R02-T03 No missed sunset gives no report and exit 0", func(t *testing.T) {
		f := newConsumersFixture(t)
		output := f.runStegoIn(t, f.project, nil, append([]string{"registry", "sunsets"}, f.paths("svc-one", "svc-two", "svc-three")...)...).accepted(t)
		for _, service := range []string{"svc-one", "svc-two", "svc-three", "svc-four"} {
			if strings.Contains(output, service) {
				t.Fatalf("sunsets names %s with no missed sunset:\n%s", service, output)
			}
		}
	})
}
