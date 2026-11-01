package assure

import "testing"

func TestResult(t *testing.T) {
	t.Run("ST-VO54-C01-T A test result is exactly passed, failed or skipped, and only passed counts as a pass", func(t *testing.T) {
		for _, text := range []string{"passed", "failed", "skipped"} {
			result, err := ParseResult(text)
			if err != nil {
				t.Fatalf("ParseResult(%q) refused it: %v", text, err)
			}
			if string(result) != text {
				t.Fatalf("ParseResult(%q) = %q", text, result)
			}
		}
		for _, text := range []string{"", "pass", "ok", "error", "Passed", "PASSED", " passed", "passed\n", "passed or failed"} {
			if result, err := ParseResult(text); err == nil {
				t.Fatalf("ParseResult(%q) accepted it as %q", text, result)
			}
		}
		if !Passed.Passes() {
			t.Fatal("passed does not count as a pass")
		}
		for _, result := range []Result{Failed, Skipped, Result("pass"), Result("")} {
			if result.Passes() {
				t.Fatalf("%q counts as a pass", result)
			}
		}
	})
}
