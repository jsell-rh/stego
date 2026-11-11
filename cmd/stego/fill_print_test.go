package main

import (
	"fmt"
	"strings"
	"testing"
)

// lineOf gives the line of the project file that first holds the text.
func lineOf(t *testing.T, p *fillProject, name, text string) int {
	t.Helper()
	for i, line := range strings.Split(string(p.read(t, name)), "\n") {
		if strings.Contains(line, text) {
			return i + 1
		}
	}
	t.Fatalf("%s does not hold %q", name, text)
	return 0
}

// callCase gives a fill case that adds one call to policy.go of a copy of untested-fill, in a function of its own
// that has a strings.Builder b, and expects the run to name the call and its line in policy.go when refused.
func callCase(t *testing.T, p *fillProject, name, call, shown string, refused bool) fillCase {
	t.Helper()
	c := fillCase{name: name, change: func(name string) {
		file := "fills/" + name + "/policy.go"
		p.replace(t, file, "import (\n\t\"context\"\n", "import (\n\t\"context\"\n\t\"fmt\"\n\t\"log\"\n\t\"log/slog\"\n\t\"strings\"\n")
		p.write(t, file, string(p.read(t, file))+"\nvar _, _, _ = fmt.Sprint, log.Print, slog.Info\n\nfunc use(sku string) {\n\tvar b strings.Builder\n\t_, _ = sku, &b\n\t"+call+"\n}\n")
	}}
	if refused {
		c.texts = []string{shown, "policy.go:"}
	}
	return c
}

// expectLines fails the test unless the run names each refused case with the line of its call in policy.go.
func expectLines(t *testing.T, p *fillProject, stdout string, cases []fillCase, calls map[string]string) {
	t.Helper()
	for _, c := range cases {
		if len(c.texts) == 0 {
			continue
		}
		at := fmt.Sprintf("policy.go:%d", lineOf(t, p, "fills/"+c.name+"/policy.go", calls[c.name]))
		if !strings.Contains(stdout, "fill "+c.name+": no evidence recorded: ") || !strings.Contains(stdout, at) {
			t.Errorf("stego test does not name %s at %s:\n%s", c.name, at, stdout)
		}
	}
}

func TestFillPrint(t *testing.T) {
	t.Parallel()
	t.Run("ST-EXT-UC01-R05-T01 stego test records no evidence for a fill that prints to standard output, and names the call", func(t *testing.T) {
		p := newFillProject(t)
		calls := map[string]string{"println-copy": `fmt.Println("reserve failed")`, "printf-copy": `fmt.Printf("reserve failed for %s", sku)`, "print-copy": `fmt.Print(sku)`}
		shown := map[string]string{"println-copy": "fmt.Println", "printf-copy": "fmt.Printf", "print-copy": "fmt.Print,"}
		var cases []fillCase
		for _, name := range []string{"println-copy", "printf-copy", "print-copy"} {
			cases = append(cases, callCase(t, p, name, calls[name], shown[name], true))
		}
		expectLines(t, p, runFillCases(t, p, cases), cases, calls)
	})

	t.Run("ST-EXT-UC01-R05-T02 stego test records no evidence for a fill that prints through a listed module", func(t *testing.T) {
		p := newFillProject(t)
		p.withHelpers(t)
		p.addImport(t, "fills/untested-fill/policy.go", "example.com/helpers/say", "say.Hello")
		before := p.read(t, "fills/untested-fill/fill.yaml")
		stdout, _, _ := p.stego(t, "no-key", "test")
		expectNoEvidence(t, p, stdout, "untested-fill", before, "example.com/helpers/say/say.go:7 uses fmt.Println")
	})

	t.Run("ST-EXT-UC01-R05-T03 stego test records evidence for a fill that formats text without printing it", func(t *testing.T) {
		p := newFillProject(t)
		cases := []fillCase{callCase(t, p, "format-copy", `_, _, _ = fmt.Sprintf("%s", sku), fmt.Errorf("reserve failed for %s", sku), func() error { _, err := fmt.Fprintf(&b, "%s", sku); return err }()`, "", false)}
		runFillCases(t, p, cases)
	})

	t.Run("ST-EXT-UC01-R07-T01 stego test records no evidence for a fill that changes, stops or bypasses the standard logger, and names the call", func(t *testing.T) {
		p := newFillProject(t)
		calls := map[string]string{
			"set-output": "log.SetOutput(&b)", "set-flags": "log.SetFlags(0)", "set-prefix": `log.SetPrefix("x")`,
			"default": "log.Default().SetOutput(&b)", "fatalf": `log.Fatalf("x")`, "panicln": `log.Panicln("x")`,
			"logger-fatal": `log.New(&b, "", 0).Fatal("x")`, "set-default": "slog.SetDefault(slog.New(slog.NewTextHandler(&b, nil)))",
			"set-level": "slog.SetLogLoggerLevel(slog.LevelDebug)", "print": `print("x")`, "println": `println("x")`,
		}
		shown := map[string]string{
			"set-output": "log.SetOutput", "set-flags": "log.SetFlags", "set-prefix": "log.SetPrefix", "default": "log.Default",
			"fatalf": "log.Fatalf", "panicln": "log.Panicln", "logger-fatal": "the method Fatal", "set-default": "log/slog.SetDefault",
			"set-level": "log/slog.SetLogLoggerLevel", "print": "the builtin print,", "println": "the builtin println",
		}
		var cases []fillCase
		for _, name := range []string{"set-output", "set-flags", "set-prefix", "default", "fatalf", "panicln", "logger-fatal", "set-default", "set-level", "print", "println"} {
			cases = append(cases, callCase(t, p, "logger-"+name, calls[name], shown[name], true))
			calls["logger-"+name] = calls[name]
		}
		expectLines(t, p, runFillCases(t, p, cases), cases, calls)
	})
}
