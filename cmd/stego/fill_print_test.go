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
		p.replace(t, file, "import (\n\t\"context\"\n", "import (\n\t\"context\"\n\t\"fmt\"\n\t\"log\"\n\t\"log/slog\"\n\t\"strings\"\n\t\"time\"\n")
		p.write(t, file, string(p.read(t, file))+"\nvar _, _, _, _ = fmt.Sprint, log.Print, slog.Info, time.Now\n\nfunc use(sku string) {\n\tvar b strings.Builder\n\t_, _ = sku, &b\n\t"+call+"\n}\n")
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
		cases = append(cases, fillCase{name: "testdata-copy", change: func(name string) {
			p.addImport(t, "fills/"+name+"/policy.go", "example.com/fills/fills/"+name+"/testdata/td", "td.P")
			p.write(t, "fills/"+name+"/testdata/td/td.go", "package td\n\nimport \"fmt\"\n\n// P prints.\nvar P = fmt.Println\n")
		}, texts: []string{"may not import example.com/fills/fills/testdata-copy/testdata/td,", "testdata/td/td.go:6 uses fmt.Println"}})
		stdout := runFillCases(t, p, cases)
		expectLines(t, p, stdout, cases[:3], calls)
	})

	t.Run("ST-EXT-UC01-R05-T02 stego test records no evidence for a fill that prints through a listed module", func(t *testing.T) {
		p := newFillProject(t)
		p.withHelpers(t)
		p.addImport(t, "fills/untested-fill/policy.go", "example.com/helpers/say", "say.Hello")
		before := p.read(t, "fills/untested-fill/fill.yaml")
		stdout, _, _ := p.stego(t, "no-key", "test")
		expectNoEvidence(t, p, stdout, "untested-fill", before, "example.com/helpers/say/say.go:7 uses fmt.Println")

		// A file of the listed module that the service build leaves out declares fmt and println, which does not hide
		// the calls of the built file.
		p.addImport(t, "fills/untested-fill-2/policy.go", "example.com/helpers/shadow", "shadow.Hello")
		before = p.read(t, "fills/untested-fill-2/fill.yaml")
		stdout, _, _ = p.stego(t, "no-key", "test")
		expectNoEvidence(t, p, stdout, "untested-fill-2", before, "example.com/helpers/shadow/s.go:7 uses fmt.Println", "example.com/helpers/shadow/s.go:8 uses the builtin println")
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
			"writer": "_ = log.Writer()", "go": "go func() {}()",
			"time-after": "time.AfterFunc(0, nil)", "context-after": "context.AfterFunc(context.Background(), nil)",
		}
		shown := map[string]string{
			"set-output": "log.SetOutput", "set-flags": "log.SetFlags", "set-prefix": "log.SetPrefix", "default": "log.Default",
			"fatalf": "log.Fatalf", "panicln": "log.Panicln", "logger-fatal": "the method Fatal", "set-default": "log/slog.SetDefault",
			"set-level": "log/slog.SetLogLoggerLevel", "print": "the builtin print,", "println": "the builtin println",
			"writer": "log.Writer", "go": "starts a goroutine with a go statement",
			"time-after": "time.AfterFunc", "context-after": "context.AfterFunc",
		}
		var cases []fillCase
		for _, name := range []string{"set-output", "set-flags", "set-prefix", "default", "fatalf", "panicln", "logger-fatal", "set-default", "set-level", "print", "println", "writer", "go", "time-after", "context-after"} {
			cases = append(cases, callCase(t, p, "logger-"+name, calls[name], shown[name], true))
			calls["logger-"+name] = calls[name]
		}
		cases = append(cases,
			fillCase{name: "logger-init", change: func(name string) {
				p.write(t, "fills/"+name+"/policy.go", string(p.read(t, "fills/"+name+"/policy.go"))+"\nfunc init() {}\n")
			}, texts: []string{"declares an init function"}},
			fillCase{name: "logger-literal", change: func(name string) {
				p.write(t, "fills/"+name+"/policy.go", string(p.read(t, "fills/"+name+"/policy.go"))+"\nvar later = []func(){func() {}}\n")
			}, texts: []string{"gives a package-level variable a function literal"}},
			fillCase{name: "logger-shadow", change: func(name string) {
				file := "fills/" + name + "/policy.go"
				p.replace(t, file, "import (\n\t\"context\"\n", "import (\n\t\"context\"\n\t\"fmt\"\n\t\"log\"\n\t\"log/slog\"\n")
				p.write(t, file, string(p.read(t, file))+"\nfunc shadowed() {\n\tfmt.Println(\"x\")\n\t_ = log.Writer()\n\tslog.SetDefault(nil)\n\tprintln(\"x\")\n}\n")
				p.write(t, "fills/"+name+"/shadow_windows.go", "package untested_fill\n\nvar fmt, log, slog int\n\nfunc println(...any) {}\n")
			}, texts: []string{"uses fmt.Println", "uses log.Writer", "uses log/slog.SetDefault", "uses the builtin println"}},
			fillCase{name: "logger-shadow-built", change: func(name string) {
				file := "fills/" + name + "/policy.go"
				p.replace(t, file, "import (\n\t\"context\"\n", "import (\n\t\"context\"\n\t\"fmt\"\n")
				p.write(t, file, string(p.read(t, file))+"\nfunc shadowed() { fmt.Println(\"x\") }\n")
				p.write(t, "fills/"+name+"/shadow.go", "package untested_fill\n\nvar fmt int\n")
			}, texts: []string{"uses fmt.Println"}},
		)
		expectLines(t, p, runFillCases(t, p, cases), cases[:len(cases)-4], calls)
	})
}
