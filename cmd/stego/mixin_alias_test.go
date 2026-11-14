package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/jsell-rh/stego/internal/compiler"
	"github.com/jsell-rh/stego/internal/registry"
)

func TestMixinAlias(t *testing.T) {
	t.Run("ST-SUP-UC10-R04-T01 Binding event-publisher gives durable-events and kafka-events", func(t *testing.T) {
		// tree gives each file of the generated output of a project and of its go.mod, by path, after stego apply.
		tree := func(service string) map[string]string {
			t.Helper()
			project := td02Project(t, service)
			if run := stegoIn(t, project, nil, "apply"); run.code != 0 {
				t.Fatalf("stego apply of %q: exit %d\n%s%s", service[strings.LastIndex(service, "mixins:"):], run.code, run.stdout, run.stderr)
			}
			files := map[string]string{}
			out := filepath.Join(project, "out")
			if err := filepath.WalkDir(out, func(path string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return err
				}
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				relative, _ := filepath.Rel(out, path)
				// The version of the declaration is the digest of its text, which names the mixins in two ways.
				files[relative] = declarationVersion.ReplaceAllString(string(data), "DeclarationVersion = \"\"")
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			mod, err := os.ReadFile(filepath.Join(project, "go.mod"))
			if err != nil {
				t.Fatal(err)
			}
			files["go.mod"] = strings.Replace(string(mod), filepath.Base(project), "project", -1)
			return files
		}
		alias := tree(td02 + "mixins: [event-publisher]\n")
		named := tree(td02 + "mixins: [durable-events, kafka-events]\n")
		if len(alias) == 0 {
			t.Fatal("the plan of event-publisher made no file")
		}
		for path, data := range alias {
			if other, ok := named[path]; !ok {
				t.Errorf("the plan of event-publisher makes %s, and the plan of durable-events and kafka-events does not", path)
			} else if other != data {
				a, b := strings.Split(data, "\n"), strings.Split(other, "\n")
				for i := range min(len(a), len(b)) {
					if a[i] != b[i] {
						t.Errorf("the plans make %s with other content, first at line %d:\n%s\n%s", path, i+1, a[i], b[i])
						break
					}
				}
			}
		}
		for path := range named {
			if _, ok := alias[path]; !ok {
				t.Errorf("the plan of durable-events and kafka-events makes %s, and the plan of event-publisher does not", path)
			}
		}
		// The base of the archetype already has the outbox, with the audit sink as its destination. The alias adds the
		// Kafka destination to the outbox and to each command, as the two mixins do, and a plan with no mixin has
		// neither.
		plain := tree(td02)
		for _, want := range []struct{ path, text string }{
			{"internal/outbox/outbox.go", `var Destinations = []string{"audit-sink", "kafka"}`},
			{"internal/outbox/outbox.go", "kafkaoutbox.New(settings, config.Logger)"},
			{"internal/api/todos_handler.go", "Destinations: []command.Destination{{Name: kafkaoutbox.DestinationName"},
		} {
			if !strings.Contains(alias[want.path], want.text) {
				t.Errorf("the plan of event-publisher has no %q in %s", want.text, want.path)
			}
			if strings.Contains(plain[want.path], want.text) {
				t.Errorf("the plan with no mixin has %q in %s, so it does not show what the alias adds", want.text, want.path)
			}
		}
	})
}

func TestMixinAliasNotice(t *testing.T) {
	t.Run("ST-SUP-UC10-R04-T02 Binding event-publisher gives a notice that names both mixins and the sunset date", func(t *testing.T) {
		shipped, err := registry.Load(filepath.Join("..", "..", "registry"))
		if err != nil {
			t.Fatal(err)
		}
		alias := shipped.Mixin("event-publisher")
		if alias == nil || alias.Sunset == "" {
			t.Fatalf("the shipped registry states no sunset of event-publisher: %+v", alias)
		}
		run := stegoIn(t, td02Project(t, td02+"mixins: [event-publisher]\n"), nil, "validate")
		if run.code != 0 {
			t.Fatalf("stego validate of event-publisher: exit %d\n%s%s", run.code, run.stdout, run.stderr)
		}
		var notices []string
		for _, line := range strings.Split(run.stdout+run.stderr, "\n") {
			if strings.Contains(line, "[deprecated-alias]") {
				notices = append(notices, line)
			}
		}
		if len(notices) != 1 {
			t.Fatalf("want one notice of the deprecated alias, got %d:\n%s%s", len(notices), run.stdout, run.stderr)
		}
		for _, text := range []string{"event-publisher", "durable-events", "kafka-events", "sunset on " + alias.Sunset} {
			if !strings.Contains(notices[0], text) {
				t.Errorf("the notice does not name %q: %s", text, notices[0])
			}
		}
	})
}

func TestEventPublisherIsADeprecatedAlias(t *testing.T) {
	t.Run("ST-SUP-INV17-T event-publisher resolves to durable-events and kafka-events, with a sunset one ga window after its deprecation", func(t *testing.T) {
		shipped, err := registry.Load(filepath.Join("..", "..", "registry"))
		if err != nil {
			t.Fatal(err)
		}
		alias := shipped.Mixin("event-publisher")
		if alias == nil || strings.Join(alias.AliasOf, " ") != "durable-events kafka-events" {
			t.Fatalf("the shipped registry gives event-publisher no alias of durable-events and kafka-events: %+v", alias)
		}
		// The ga window comes from the registry's own policy file, read here apart from the loader, so a change of the
		// window moves the expected sunset.
		data, err := os.ReadFile(filepath.Join("..", "..", "registry", "policy.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		var policy registry.Policy
		if err := yaml.Unmarshal(data, &policy); err != nil {
			t.Fatal(err)
		}
		window, ok := policy.Windows["ga"]
		if !ok || window <= 0 {
			t.Fatalf("the registry policy states no ga window: %+v", policy)
		}
		deprecated, err := time.Parse(time.DateOnly, alias.DeprecatedOn)
		if err != nil {
			t.Fatalf("event-publisher states no deprecated_on date: %v", err)
		}
		if want := deprecated.AddDate(0, window, 0).Format(time.DateOnly); alias.Sunset != want {
			t.Fatalf("event-publisher has the sunset %q; deprecated_on %s plus the ga window of %d months is %s", alias.Sunset, alias.DeprecatedOn, window, want)
		}

		// plan applies a declaration and gives its generated files, its active components and its notices of a
		// deprecated alias from stego validate.
		plan := func(mixins string) (map[string]string, []string, []string) {
			t.Helper()
			project := td02Project(t, td02+mixins)
			check := stegoIn(t, project, nil, "validate")
			if check.code != 0 {
				t.Fatalf("stego validate of %q: exit %d\n%s%s", mixins, check.code, check.stdout, check.stderr)
			}
			var notices []string
			for _, line := range strings.Split(check.stdout+check.stderr, "\n") {
				if strings.Contains(line, "[deprecated-alias]") {
					notices = append(notices, line)
				}
			}
			if run := stegoIn(t, project, nil, "apply"); run.code != 0 {
				t.Fatalf("stego apply of %q: exit %d\n%s%s", mixins, run.code, run.stdout, run.stderr)
			}
			state, err := compiler.LoadState(filepath.Join(project, ".stego", "state.yaml"))
			if err != nil || state.LastApplied == nil {
				t.Fatalf("the apply of %q left no state: %v", mixins, err)
			}
			var components []string
			for name := range state.LastApplied.Components {
				components = append(components, name)
			}
			slices.Sort(components)
			files := map[string]string{}
			out := filepath.Join(project, "out")
			if err := filepath.WalkDir(out, func(path string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return err
				}
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				relative, _ := filepath.Rel(out, path)
				// The version of the declaration is the digest of its text, which names the mixins in two ways.
				files[relative] = declarationVersion.ReplaceAllString(string(data), "DeclarationVersion = \"\"")
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			return files, components, notices
		}
		aliasFiles, aliasComponents, aliasNotices := plan("mixins: [event-publisher]\n")
		namedFiles, namedComponents, namedNotices := plan("mixins: [durable-events, kafka-events]\n")
		if !slices.Equal(aliasComponents, namedComponents) || !slices.Contains(aliasComponents, "kafka-producer") {
			t.Errorf("the active components differ: event-publisher gives %v, durable-events and kafka-events give %v", aliasComponents, namedComponents)
		}
		if len(aliasFiles) == 0 || len(aliasFiles) != len(namedFiles) {
			t.Errorf("event-publisher makes %d files, durable-events and kafka-events make %d", len(aliasFiles), len(namedFiles))
		}
		for path, data := range aliasFiles {
			if namedFiles[path] != data {
				t.Errorf("the two plans make %s with other content, or only one makes it", path)
			}
		}
		if len(aliasNotices) != 1 {
			t.Fatalf("want one notice for event-publisher, got %v", aliasNotices)
		}
		for _, text := range []string{"durable-events", "kafka-events", alias.Sunset} {
			if !strings.Contains(aliasNotices[0], text) {
				t.Errorf("the notice does not name %q: %s", text, aliasNotices[0])
			}
		}
		if len(namedNotices) != 0 {
			t.Errorf("want no notice for durable-events and kafka-events, got %v", namedNotices)
		}
	})
}

// declarationVersion is the line of the generated code that holds the digest of the declaration's text.
var declarationVersion = regexp.MustCompile(`DeclarationVersion = "[0-9a-f]{64}"`)
