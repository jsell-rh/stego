package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// These tests run stego lifecycle supersede and stego lifecycle withdraw through the stego binary, as a platform
// engineer runs them in a registry checkout. The registry is a copy of the repository's registry, committed in a
// Git repository of its own, as in ST-GEN-TD03. Each lifecycle date is relative to the day T the test runs.

// lifecycleDigest gives a component digest of 64 lower-case hexadecimal digits from one digit.
func lifecycleDigest(digit string) string {
	return strings.Repeat(digit, 64)
}

// lifecycleDay is the day T plus the given days, written as YYYY-MM-DD.
func lifecycleDay(days int) string {
	return time.Now().UTC().AddDate(0, 0, days).Format("2006-01-02")
}

// registryCopy copies the repository's registry into a new Git repository and gives its folder. It records one
// withdrawal of another digest first, so the lifecycle folder exists and holds an advisory that a refused command
// must keep as it is.
func registryCopy(t *testing.T) string {
	t.Helper()
	stegoBinary(t)
	_, source, _, _ := runtime.Caller(0)
	dir := filepath.Join(t.TempDir(), "registry")
	if output, err := exec.Command("cp", "-R", filepath.Join(filepath.Dir(source), "../../registry"), dir).CombinedOutput(); err != nil {
		t.Fatalf("copy the registry: %v\n%s", err, output)
	}
	mustStego(t, "lifecycle", "withdraw", "--registry", dir, "--component", "health-check",
		"--digest", lifecycleDigest("9"), "--fails-after", lifecycleDay(60), "--reason", "an earlier finding")
	git(t, dir, "init", "-q")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "R1")
	return dir
}

// folderState is the listing of the lifecycle folder with the bytes of each file.
func folderState(t *testing.T, registry string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(registry, "lifecycle"))
	if err != nil {
		t.Fatal(err)
	}
	state := map[string]string{}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(registry, "lifecycle", entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		state[entry.Name()] = string(data)
	}
	return state
}

// refusedRun runs stego and expects it to fail, to name each text, and to leave the lifecycle folder as it was.
func refusedRun(t *testing.T, registry string, wants []string, args ...string) {
	t.Helper()
	before := folderState(t, registry)
	stdout, stderr, err := runStego(t, args...)
	if err == nil {
		t.Fatalf("stego %s exits 0; want a refusal\n%s", strings.Join(args, " "), stdout)
	}
	for _, want := range wants {
		if !strings.Contains(stderr, want) {
			t.Errorf("stego %s does not name %q:\n%s", strings.Join(args, " "), want, stderr)
		}
	}
	after := folderState(t, registry)
	if len(after) != len(before) {
		t.Fatalf("the refusal changed the listing of the lifecycle folder: %v, was %v", keys(after), keys(before))
	}
	for name, data := range before {
		if after[name] != data {
			t.Fatalf("the refusal changed %s", name)
		}
	}
}

func keys(m map[string]string) []string {
	var result []string
	for key := range m {
		result = append(result, key)
	}
	slices.Sort(result)
	return result
}

func TestSupersedeNamesWhatIsMissing(t *testing.T) {
	t.Run("ST-DIS-UC04-R02-T02 A supersession with no successor or no fail date is refused, and nothing is recorded", func(t *testing.T) {
		registry := registryCopy(t)
		base := []string{"lifecycle", "supersede", "--registry", registry, "--component", "health-check", "--digest", lifecycleDigest("1"),
			"--needs-declaration-change", "no", "--needs-fill-change", "no", "--summary", "the fix of the health check"}
		refusedRun(t, registry, []string{"--successor"}, append(slices.Clone(base), "--fails-after", lifecycleDay(90))...)
		refusedRun(t, registry, []string{"--fails-after"}, append(slices.Clone(base), "--successor", lifecycleDigest("2"))...)
	})
	t.Run("ST-DIS-UC04-R03-T01 A supersession that does not state whether a declaration or fill change is needed is refused", func(t *testing.T) {
		registry := registryCopy(t)
		base := []string{"lifecycle", "supersede", "--registry", registry, "--component", "health-check", "--digest", lifecycleDigest("1"),
			"--successor", lifecycleDigest("2"), "--fails-after", lifecycleDay(90), "--summary", "the fix of the health check"}
		refusedRun(t, registry, []string{"--needs-declaration-change"}, append(slices.Clone(base), "--needs-fill-change", "no")...)
		refusedRun(t, registry, []string{"--needs-fill-change"}, append(slices.Clone(base), "--needs-declaration-change", "no")...)
	})
}

func TestWithdrawCarriesAFutureDate(t *testing.T) {
	t.Run("ST-DIS-UC07-R02-T02 A withdrawal whose date is already past is refused, and nothing is recorded", func(t *testing.T) {
		registry := registryCopy(t)
		past := lifecycleDay(-1)
		refusedRun(t, registry, []string{past, "already past"}, "lifecycle", "withdraw", "--registry", registry,
			"--component", "health-check", "--digest", lifecycleDigest("b"), "--reason", "leaks a token in a log", "--fails-after", past)
	})
	t.Run("ST-DIS-UC07-R02-T03 A withdrawal with no date is refused, and nothing is recorded", func(t *testing.T) {
		registry := registryCopy(t)
		refusedRun(t, registry, []string{"--fails-after", "date"}, "lifecycle", "withdraw", "--registry", registry,
			"--component", "health-check", "--digest", lifecycleDigest("b"), "--reason", "leaks a token in a log")
	})
}
