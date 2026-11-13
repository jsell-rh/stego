package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/compiler"
	"github.com/jsell-rh/stego/internal/registry"
)

// These tests drive validate, plan and apply through the stego binary on the small service of ST-GEN-TD03, with
// the lifecycle records of ST-DIS-TD01 committed at the registry's head: the registry repository's main branch,
// while the service's registry pin stays at R1. Each lifecycle date is relative to the day T the test runs.

// headRegistry puts the registry repository on its main branch, the head that STEGO reads the lifecycle from.
func headRegistry(t *testing.T, s *smallService) {
	t.Helper()
	git(t, s.registry, "checkout", "-q", "main")
}

// commitHead commits the lifecycle folder at the registry's head.
func commitHead(t *testing.T, s *smallService, message string) {
	t.Helper()
	git(t, s.registry, "add", "-A", "lifecycle")
	git(t, s.registry, "commit", "-q", "--allow-empty", "-m", message)
}

// pinnedDigest is the component digest the service's record holds for the component.
func pinnedDigest(t *testing.T, s *smallService, component string) string {
	t.Helper()
	applied, _ := s.record(t)["last_applied"].(map[string]any)
	pins, _ := applied["components"].(map[string]any)
	pin, _ := pins[component].(map[string]any)
	digest, _ := pin["sha"].(string)
	if len(digest) != 64 {
		t.Fatalf("the record holds no component digest for %s: %v", component, pin)
	}
	return digest
}

// otherComponent is a component of the active set other than component A: component B of ST-DIS-TD01.
func otherComponent(t *testing.T, s *smallService) string {
	t.Helper()
	applied, _ := s.record(t)["last_applied"].(map[string]any)
	pins, _ := applied["components"].(map[string]any)
	for _, name := range []string{"health-check", "http-application", "postgres-adapter"} {
		if pins[name] != nil {
			return name
		}
	}
	for name := range pins {
		if name != componentA {
			return name
		}
	}
	t.Fatal("the record pins no component other than A")
	return ""
}

// digestAt is the component digest of the component in the registry at the commit.
func digestAt(t *testing.T, s *smallService, commit, component string) string {
	t.Helper()
	files, err := registry.CaptureInputFiles(s.registryAt(t, commit))
	if err != nil {
		t.Fatal(err)
	}
	digest, err := compiler.ComponentDigest(files, component)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

// supersede writes the standing variant of ST-DIS-TD01 (or its past form) at the registry's head: DA1 superseded
// by DA2, failing after T plus the days given. stego lifecycle supersede needs a component stage that the
// registry does not state yet, so the fixture writes the advisory as the command writes one.
func supersede(t *testing.T, s *smallService, id, from, to string, days int, declaration bool) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(s.registry, "lifecycle"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(s.registry, "lifecycle"), id+".json", fmt.Sprintf(`{
  "schema_version": "1.6.0",
  "id": "%s",
  "modified": "2026-11-01T09:00:00Z",
  "published": "2026-11-01T09:00:00Z",
  "summary": "the fix of component A",
  "affected": [{"package": {"ecosystem": "STEGO", "name": "%s"},
    "ranges": [{"type": "ECOSYSTEM", "events": [{"introduced": "%s"}, {"fixed": "%s"}]}]}],
  "database_specific": {"stego": {"kind": "supersession", "fails_after": "%s",
    "needs_declaration_change": %v, "needs_fill_change": false}}
}
`, id, componentA, from, to, lifecycleDay(days), declaration), 0o644)
	commitHead(t, s, "supersede "+id)
}

// withdraw records a withdrawal with stego lifecycle withdraw at the registry's head, failing after T+60, and,
// for the past form, moves its fail date to T plus the days given, as ST-DIS-TD01's withdrawn past variant does.
func withdraw(t *testing.T, s *smallService, component, digest string, pastDays int) string {
	t.Helper()
	out := s.must(t, "lifecycle", "withdraw", "--registry", s.registry, "--component", component, "--digest", digest,
		"--fails-after", lifecycleDay(60), "--reason", "leaks a token in a log")
	id := strings.TrimSuffix(strings.Fields(out)[2], ":")
	if pastDays != 0 {
		file := filepath.Join(s.registry, "lifecycle", id+".json")
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Dir(file), filepath.Base(file), strings.Replace(string(data), lifecycleDay(60), lifecycleDay(pastDays), 1), 0o644)
	}
	commitHead(t, s, "withdraw "+id)
	return id
}

// refusedIn runs stego in the service, expects it to fail and to name each text, and expects no file of the
// service to change.
func refusedIn(t *testing.T, s *smallService, wants []string, args ...string) string {
	t.Helper()
	before := tree(t, s.service)
	stdout, stderr, err := s.stego(t, args...)
	if err == nil {
		t.Fatalf("stego %s exits 0; want a refusal\n%s", strings.Join(args, " "), stdout)
	}
	for _, want := range wants {
		if !strings.Contains(stdout+stderr, want) {
			t.Errorf("stego %s does not name %q:\n%s%s", strings.Join(args, " "), want, stdout, stderr)
		}
	}
	if after := tree(t, s.service); !mapsEqual(before, after) {
		t.Errorf("stego %s changed a file of the service", strings.Join(args, " "))
	}
	return stdout + stderr
}

func TestANewDigestAloneSupersedesNothing(t *testing.T) {
	t.Parallel()
	t.Run("ST-DIS-UC04-R02-T03 A new digest alone supersedes nothing", func(t *testing.T) {
		s := newSmallService(t)
		headRegistry(t, s)
		da1 := pinnedDigest(t, s, componentA)
		if da2 := digestAt(t, s, s.r2, componentA); da2 == da1 {
			t.Fatal("R2 does not give component A a new digest")
		}
		out := s.must(t, "validate")
		if !strings.Contains(out, "current "+componentA+" "+da1) {
			t.Fatalf("validate does not report DA1 as current:\n%s", out)
		}
		entries, err := os.ReadDir(filepath.Join(s.registry, "lifecycle"))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			data, _ := os.ReadFile(filepath.Join(s.registry, "lifecycle", entry.Name()))
			if strings.Contains(string(data), da1) {
				t.Fatalf("the lifecycle folder holds a record for DA1: %s", entry.Name())
			}
		}
	})
}

func TestPastItsFailDateAPinFails(t *testing.T) {
	t.Parallel()
	t.Run("ST-DIS-UC05-R03-T01 Past its fail date, validate fails for the pin", func(t *testing.T) {
		s := newSmallService(t)
		headRegistry(t, s)
		da1, da2 := pinnedDigest(t, s, componentA), digestAt(t, s, s.r2, componentA)
		supersede(t, s, "STEGO-2026-0001", da1, da2, -30, false)
		refusedIn(t, s, []string{da1, da2, lifecycleDay(-30), "past its fail date"}, "validate")
	})
	t.Run("ST-DIS-UC05-R03-T02 Past its fail date, apply writes no output without an override", func(t *testing.T) {
		s := newSmallService(t)
		s.unreleased(t)
		headRegistry(t, s)
		da1, da2 := pinnedDigest(t, s, componentA), digestAt(t, s, s.r2, componentA)
		supersede(t, s, "STEGO-2026-0001", da1, da2, -30, false)
		s.write(t, "service.yaml", secondEntityDeclaration)
		git(t, s.service, "commit", "-q", "-am", "add Tag")
		refusedIn(t, s, []string{da1}, "apply")
	})
}

func TestAnUnreadRegistryIsNotAPass(t *testing.T) {
	t.Parallel()
	t.Run("ST-DIS-UC05-R04-T01 When the lifecycle records cannot be read, validate and plan say so and report no pin as current", func(t *testing.T) {
		s := newSmallService(t)
		headRegistry(t, s)
		writeFile(t, filepath.Join(s.registry, "lifecycle"), "STEGO-2026-0001.json", "{ not an advisory\n", 0o644)
		commitHead(t, s, "the unreadable variant")
		for _, command := range []string{"validate", "plan"} {
			out := refusedIn(t, s, []string{"Could not check the standing of the pins", "STEGO-2026-0001.json"}, command)
			if strings.Contains(out, "current ") {
				t.Errorf("stego %s reports a pin as current:\n%s", command, out)
			}
		}
		git(t, s.registry, "rm", "-q", "-r", "lifecycle")
		git(t, s.registry, "commit", "-q", "-m", "the lifecycle folder removed")
		for _, command := range []string{"validate", "plan"} {
			out := refusedIn(t, s, []string{"Could not check the standing of the pins", "no lifecycle folder"}, command)
			if strings.Contains(out, "current ") {
				t.Errorf("stego %s reports a pin as current:\n%s", command, out)
			}
		}
	})
}

func TestAWithdrawalFailsAfterItsDate(t *testing.T) {
	t.Parallel()
	t.Run("ST-DIS-UC07-R02-T01 A withdrawal with a future date is recorded, and after the date validate fails", func(t *testing.T) {
		s := newSmallService(t)
		headRegistry(t, s)
		b := otherComponent(t, s)
		db1 := pinnedDigest(t, s, b)
		out := s.must(t, "lifecycle", "withdraw", "--registry", s.registry, "--component", b, "--digest", db1,
			"--fails-after", lifecycleDay(30), "--reason", "leaks a token in a log")
		id := strings.TrimSuffix(strings.Fields(out)[2], ":")
		file := filepath.Join(s.registry, "lifecycle", id+".json")
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{db1, "leaks a token in a log", lifecycleDay(30), `"name": "` + b + `"`} {
			if !strings.Contains(string(data), want) {
				t.Errorf("the lifecycle record does not hold %q:\n%s", want, data)
			}
		}
		// The record's date moves to T-10 days, as in the withdrawn past variant of ST-DIS-TD01.
		writeFile(t, filepath.Dir(file), filepath.Base(file), strings.Replace(string(data), lifecycleDay(30), lifecycleDay(-10), 1), 0o644)
		commitHead(t, s, "withdraw "+id)
		refusedIn(t, s, []string{db1, "withdrawn", "past its fail date"}, "validate")
	})
}

func TestAWithdrawnDigestTakesNoNewPin(t *testing.T) {
	t.Parallel()
	t.Run("ST-DIS-UC07-R03-T01 A new pin to a withdrawn digest is refused at once, whatever its date", func(t *testing.T) {
		s := newSmallService(t)
		headRegistry(t, s)
		// The digest of component A at R2 is withdrawn, failing after T+60. The service holds DA1 and does not
		// pin it yet; its registry pin moves to R2, so the run takes it as a new pin.
		withdrawnDigest := digestAt(t, s, s.r2, componentA)
		if strings.Contains(s.read(t, ".stego/state.yaml"), withdrawnDigest) {
			t.Fatal("the service already holds the withdrawn digest")
		}
		withdraw(t, s, componentA, withdrawnDigest, 0)
		s.pin(t, s.r2)
		git(t, s.service, "commit", "-q", "-am", "pin R2")
		for _, command := range []string{"plan", "apply"} {
			refusedIn(t, s, []string{withdrawnDigest, "withdrawn", "leaks a token in a log", "new pin to a withdrawn digest"}, command)
		}
	})
}

func TestAServiceAuthorLearnsOfASupersededPin(t *testing.T) {
	t.Parallel()
	t.Run("ST-DIS-UC05-FL-T A service author learns of a superseded pin while working, and past its date the pin fails", func(t *testing.T) {
		s := newSmallService(t)
		s.unreleased(t)
		headRegistry(t, s)
		da1, da2 := pinnedDigest(t, s, componentA), digestAt(t, s, s.r2, componentA)
		supersede(t, s, "STEGO-2026-0001", da1, da2, 90, false)
		for _, command := range []string{"validate", "plan"} {
			out := s.must(t, command)
			want := "superseded " + componentA + " " + da1 + " by " + da2 + ", fails after " + lifecycleDay(90)
			if !strings.Contains(out, want) {
				t.Errorf("stego %s does not report %q:\n%s", command, want, out)
			}
		}
		// The variant changes to past.
		supersedeFile := filepath.Join(s.registry, "lifecycle", "STEGO-2026-0001.json")
		data, err := os.ReadFile(supersedeFile)
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Dir(supersedeFile), filepath.Base(supersedeFile), strings.Replace(string(data), lifecycleDay(90), lifecycleDay(-30), 1), 0o644)
		commitHead(t, s, "the past variant")
		refusedIn(t, s, []string{da1, "past its fail date"}, "validate")
		s.write(t, "service.yaml", secondEntityDeclaration)
		git(t, s.service, "commit", "-q", "-am", "add Tag")
		refusedIn(t, s, []string{da1}, "apply")
		// On the unchecked path, the lifecycle records cannot be read.
		writeFile(t, filepath.Dir(supersedeFile), "STEGO-2026-0002.json", "{ not an advisory\n", 0o644)
		commitHead(t, s, "the unreadable variant")
		out := refusedIn(t, s, []string{"Could not check the standing of the pins"}, "validate")
		if strings.Contains(out, "current ") {
			t.Errorf("validate reports a pin as current:\n%s", out)
		}
	})
}

// recordDifferences gives the dotted paths of the record whose values differ between two records.
func recordDifferences(before, after any, prefix string) []string {
	beforeMap, isMap := before.(map[string]any)
	afterMap, isMap2 := after.(map[string]any)
	if !isMap || !isMap2 {
		if fmt.Sprint(before) != fmt.Sprint(after) {
			return []string{prefix}
		}
		return nil
	}
	var paths []string
	keys := map[string]bool{}
	for key := range beforeMap {
		keys[key] = true
	}
	for key := range afterMap {
		keys[key] = true
	}
	for key := range keys {
		paths = append(paths, recordDifferences(beforeMap[key], afterMap[key], strings.TrimPrefix(prefix+"."+key, "."))...)
	}
	return paths
}

func TestAComponentFixCarriesNoUnrelatedChanges(t *testing.T) {
	t.Parallel()
	t.Run("ST-DIS-UC04-R01-T01 A consumer takes a component fix with no change outside the fixed component and no compiler change", func(t *testing.T) {
		s := newSmallService(t)
		headRegistry(t, s)
		// The first apply of ST-GEN-TD03 records go.mod as an input that does not exist yet, and writes it, so
		// a second apply at R1 updates the record with no fix at all (reported to Generating). The service takes
		// that update first, so the diff below shows the fix alone.
		s.must(t, "apply")
		git(t, s.service, "add", "-A")
		git(t, s.service, "commit", "-q", "--allow-empty", "-m", "settle the record at R1")
		if plan := s.must(t, "plan"); !strings.Contains(plan, "No changes") && !strings.Contains(plan, "0 to generate, 0 to update, 0 to delete") {
			t.Fatalf("the record at R1 does not settle:\n%s", plan)
		}
		before := s.record(t)
		da1 := pinnedDigest(t, s, componentA)
		s.pin(t, s.r2)
		s.must(t, "apply")
		changed := strings.Fields(git(t, s.service, "diff", "--name-only"))
		// The record keys each generated file by its path under out/. On the runtime data path the fix of
		// component A lands in the runtime wiring, which reads its settings, and in no other file.
		fixed := 0
		for _, path := range changed {
			switch {
			case path == ".stego/state.yaml" || path == ".stego/config.yaml":
			case path == "out/"+componentAFile:
				fixed++
			default:
				t.Errorf("the fix of component A changed %s, which is not out/%s or the record", path, componentAFile)
			}
		}
		if fixed == 0 {
			t.Errorf("the fix of component A did not change out/%s: %v", componentAFile, changed)
		}
		after := s.record(t)
		for _, path := range recordDifferences(before, after, "") {
			switch {
			// The registry reference is the pin in .stego/config.yaml, which the record captures as an input, the
			// registry's ref and its content hash.
			case path == "last_applied.components."+componentA+".sha", path == "last_applied.registry_sha",
				path == "last_applied.inputs.options.registry_ref", path == "last_applied.registry_content_sha256",
				path == "last_applied.inputs.files..stego/config.yaml.sha256", path == "last_applied.inputs.sha256":
			// The hash the record keeps of the file that the fix changes is part of the fix.
			case path == "last_applied.files."+componentAFile:
			default:
				t.Errorf("the record changed %s, which is neither component A's digest, the file of its fix, nor the registry reference", path)
			}
		}
		if pinnedDigest(t, s, componentA) == da1 {
			t.Error("the record keeps DA1 after the pin moved to R2")
		}
		beforeBuild, _ := before["last_applied"].(map[string]any)["compiler_build"].(map[string]any)
		afterBuild, _ := after["last_applied"].(map[string]any)["compiler_build"].(map[string]any)
		if fmt.Sprint(beforeBuild) != fmt.Sprint(afterBuild) {
			t.Errorf("the compiler build changed: %v, was %v", afterBuild, beforeBuild)
		}
	})
}
