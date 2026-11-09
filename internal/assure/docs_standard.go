package assure

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
)

// The rules of STEGO's user-doc standard (ST-STD-DOCS) that the doc predicates hold a tree to.
const (
	RuleDocInventory       = "ST-STD-DOCS-R01"
	RuleDocNamesResolve    = "ST-STD-DOCS-R02"
	RuleDocSectionsCurrent = "ST-STD-DOCS-R03"
	RuleDocFollowsChange   = "ST-STD-DOCS-R04"
)

// DocInventoryRecord is the tooling record that names the user doc inventory (ST-STD-DOCS-R01): one path from the
// root per line, a line that starts with # being a comment. A tree whose spec has no such record has no user docs.
const DocInventoryRecord = "ST-ASR-TOOL05"

// CLICommand is one CLI command record of the spec: its ID, its title (the invocation, such as "stego fill sign"),
// its usage and description, and the files of the CLI handlers that run it.
type CLICommand struct {
	ID          string
	Title       string
	Usage       string
	Description string
	Handlers    []string
}

// readCLICommands reads the CLI command records from the cli_command table, with the files of the cli_handler
// records that the runs table links to each, in ID order. A row that is not a JSON object, or that names no ID, is
// passed over.
func readCLICommands(tables map[string]SpecFile) []CLICommand {
	rows := func(table string) []map[string]string {
		file, ok := tables[table]
		if !ok {
			return nil
		}
		var read []map[string]string
		for _, line := range bytes.Split(file.Data, []byte("\n")) {
			var fields map[string]json.RawMessage
			if len(bytes.TrimSpace(line)) == 0 || json.Unmarshal(line, &fields) != nil {
				continue
			}
			row := make(map[string]string, len(fields))
			for name, raw := range fields {
				var value string
				if json.Unmarshal(raw, &value) == nil {
					row[name] = value
				}
			}
			read = append(read, row)
		}
		return read
	}
	handlerFiles := make(map[string]string)
	for _, row := range rows("cli_handler") {
		if row["id"] != "" && row["file"] != "" {
			handlerFiles[row["id"]] = normalPath(row["file"])
		}
	}
	handlers := make(map[string][]string)
	for _, link := range rows("runs") {
		if file, ok := handlerFiles[link["source_id"]]; ok && link["target_id"] != "" {
			handlers[link["target_id"]] = append(handlers[link["target_id"]], file)
		}
	}
	var commands []CLICommand
	for _, row := range rows("cli_command") {
		if row["id"] == "" || row["title"] == "" {
			continue
		}
		files := handlers[row["id"]]
		sort.Strings(files)
		commands = append(commands, CLICommand{
			ID: row["id"], Title: strings.Join(strings.Fields(row["title"]), " "), Usage: row["usage"],
			Description: row["description"], Handlers: files,
		})
	}
	sort.Slice(commands, func(i, j int) bool { return commands[i].ID < commands[j].ID })
	return commands
}

// built tells whether a CLI handler runs the command and the tree holds the handler's file.
func (c CLICommand) built(tree Tree) bool {
	for _, file := range c.Handlers {
		if _, ok := tree.File(file); ok {
			return true
		}
	}
	return false
}

// UserDocs reads the user doc inventory of the tree (ST-STD-DOCS-R01): the paths that the file of the record
// DocInventoryRecord lists, in the order it lists them. It gives no paths when the spec has no such record or the
// tree holds no such file.
func UserDocs(spec Spec, tree Tree) []string {
	for _, record := range spec.Technical {
		if record.ID != DocInventoryRecord {
			continue
		}
		data, ok := tree.File(record.File)
		if !ok {
			return nil
		}
		var docs []string
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			docs = append(docs, normalPath(line))
		}
		return docs
	}
	return nil
}

// docInventoryFaults is the predicate of ST-ASR-SP14 (ST-STD-DOCS-R01): the inventory lists a path that the tree
// does not hold, or a path that is not a Markdown file.
func docInventoryFaults(spec Spec, tree Tree) []CodeFault {
	var faults []CodeFault
	for _, doc := range UserDocs(spec, tree) {
		if _, ok := tree.File(doc); !ok {
			faults = append(faults, CodeFault{Rule: RuleDocInventory, Key: doc, Path: doc, Reason: "the user doc inventory lists the file, and the tree does not hold it"})
		} else if path.Ext(doc) != ".md" {
			faults = append(faults, CodeFault{Rule: RuleDocInventory, Key: doc, Path: doc, Reason: "the user doc inventory lists a file that is not Markdown"})
		}
	}
	sortFaults(faults)
	return faults
}

// The markers of a generated section of a user doc (ST-STD-DOCS-R03).
const (
	docSectionStart = "<!-- stego:generated "
	docSectionEnd   = "<!-- stego:end -->"
)

var docSectionPattern = regexp.MustCompile(`(?s)<!-- stego:generated ([a-z0-9-]+) -->\n(.*?)` + regexp.QuoteMeta(docSectionEnd))

// docProse is the text of a doc with each generated section's body taken out, so that a predicate that reads what
// the doc's own text names does not read the text that a generator wrote.
func docProse(text string) string {
	return docSectionPattern.ReplaceAllString(text, "")
}

// docGenerators are the generators of the generated sections, by name: each gives a section's body from the spec
// and the tree at one commit.
func docGenerators() map[string]func(Spec, Tree) string {
	return map[string]func(Spec, Tree) string{"cli-commands": cliCommandTable}
}

// cliCommandTable is the generated section cli-commands: a Markdown table of each built stego command, in title
// order, with its usage.
func cliCommandTable(spec Spec, tree Tree) string {
	var commands []CLICommand
	for _, command := range spec.Commands {
		if strings.HasPrefix(command.Title, "stego ") && command.built(tree) {
			commands = append(commands, command)
		}
	}
	sort.Slice(commands, func(i, j int) bool { return commands[i].Title < commands[j].Title })
	var b strings.Builder
	b.WriteString("| Command | Usage |\n|---|---|\n")
	for _, command := range commands {
		usage := strings.ReplaceAll(strings.Join(strings.Fields(command.Usage), " "), "|", `\|`)
		fmt.Fprintf(&b, "| `%s` | `%s` |\n", command.Title, usage)
	}
	return b.String()
}

// RegenerateDocSections gives the doc's text with the body of each generated section written again from the spec
// and the tree, and the names of the sections whose generator does not exist, which it leaves as they are.
func RegenerateDocSections(spec Spec, tree Tree, text string) (string, []string) {
	generators := docGenerators()
	var unknown []string
	out := docSectionPattern.ReplaceAllStringFunc(text, func(section string) string {
		name := docSectionPattern.FindStringSubmatch(section)[1]
		generate, ok := generators[name]
		if !ok {
			unknown = append(unknown, name)
			return section
		}
		return docSectionStart + name + " -->\n" + generate(spec, tree) + docSectionEnd
	})
	return out, unknown
}

// docSectionStale is the predicate of ST-ASR-SP13 (ST-STD-DOCS-R03): a generated section of a user doc differs from
// what its generator gives from the spec and the tree at the same commit, or names no generator, or has a start
// marker with no end marker.
func docSectionStale(spec Spec, tree Tree) []CodeFault {
	var faults []CodeFault
	for _, doc := range UserDocs(spec, tree) {
		data, ok := tree.File(doc)
		if !ok {
			continue
		}
		text := string(data)
		if strings.Count(text, docSectionStart) != len(docSectionPattern.FindAllString(text, -1)) {
			faults = append(faults, CodeFault{Rule: RuleDocSectionsCurrent, Key: doc, Path: doc, Reason: "a generated section has a start marker with no end marker"})
		}
		generators := docGenerators()
		for _, match := range docSectionPattern.FindAllStringSubmatch(text, -1) {
			name, body := match[1], match[2]
			generate, known := generators[name]
			switch {
			case !known:
				faults = append(faults, CodeFault{Rule: RuleDocSectionsCurrent, Key: doc + " " + name, Path: doc, Reason: "the generated section " + name + " names no generator"})
			case body != generate(spec, tree):
				faults = append(faults, CodeFault{Rule: RuleDocSectionsCurrent, Key: doc + " " + name, Path: doc,
					Reason: "the generated section " + name + " is not what the spec gives; write it again with go test ./internal/assure -run TestOwnTree -update-docs"})
			}
		}
	}
	sortFaults(faults)
	return faults
}

// DocCommand is one stego, or other, command line that a user doc names, as written: the words before the first
// flag, then its flags without their leading dashes and values.
type DocCommand struct {
	Text  string
	Words []string
	Flags []string
}

var (
	fencePattern      = regexp.MustCompile("(?s)```[a-z]*\n(.*?)```")
	inlineCodePattern = regexp.MustCompile("`([^`\n]+)`")
	usageFlagPattern  = regexp.MustCompile(`(?:^|[\s\[(|])--?([a-zA-Z][a-zA-Z0-9-]*)`)
	linkPattern       = regexp.MustCompile(`\]\(([^)\s]+)\)`)
)

// docCommands reads the command lines that the doc's prose names for the program: each line of a fenced block, and
// each inline code span, that starts with the program's name and a space, cut at a shell comment.
func docCommands(text, program string) []DocCommand {
	prose := docProse(text)
	var lines []string
	for _, block := range fencePattern.FindAllStringSubmatch(prose, -1) {
		lines = append(lines, strings.Split(block[1], "\n")...)
	}
	outside := fencePattern.ReplaceAllString(prose, "")
	for _, span := range inlineCodePattern.FindAllStringSubmatch(outside, -1) {
		lines = append(lines, span[1])
	}
	var commands []DocCommand
	for _, line := range lines {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "$ "))
		if cut, _, found := strings.Cut(line, " #"); found {
			line = strings.TrimSpace(cut)
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != program {
			continue
		}
		command := DocCommand{Text: strings.Join(fields, " ")}
		inWords := true
		for _, field := range fields[1:] {
			if field == "--" {
				break
			}
			if strings.HasPrefix(field, "-") {
				inWords = false
				name, _, _ := strings.Cut(strings.TrimLeft(field, "-"), "=")
				if name != "" {
					command.Flags = append(command.Flags, name)
				}
				continue
			}
			if inWords {
				command.Words = append(command.Words, field)
			}
		}
		commands = append(commands, command)
	}
	return commands
}

// matchCommand is the CLI command record whose title is the longest run of the command's first words, or false.
func matchCommand(commands []CLICommand, program string, words []string) (CLICommand, bool) {
	for n := len(words); n >= 0; n-- {
		title := strings.Join(append([]string{program}, words[:n]...), " ")
		for _, command := range commands {
			if command.Title == title {
				return command, true
			}
		}
	}
	return CLICommand{}, false
}

// usageFlags are the flag names that a usage line lists, without their dashes, with help, which every command takes.
func usageFlags(usage string) map[string]bool {
	flags := map[string]bool{"help": true, "h": true}
	for _, match := range usageFlagPattern.FindAllStringSubmatch(usage, -1) {
		flags[match[1]] = true
	}
	return flags
}

// docNamesUnknownCommand is the predicate of ST-ASR-SP11 (ST-STD-DOCS-R02): a user doc names a stego command that
// no built CLI command record is, or gives a command a flag that its usage does not list.
func docNamesUnknownCommand(spec Spec, tree Tree) []CodeFault {
	var faults []CodeFault
	for _, doc := range UserDocs(spec, tree) {
		data, ok := tree.File(doc)
		if !ok {
			continue
		}
		for _, line := range docCommands(string(data), "stego") {
			command, found := matchCommand(spec.Commands, "stego", line.Words)
			switch {
			case !found:
				faults = append(faults, CodeFault{Rule: RuleDocNamesResolve, Key: doc + " " + line.Text, Path: doc,
					Reason: "the doc names " + line.Text + ", and no CLI command record has that title"})
				continue
			case !command.built(tree):
				faults = append(faults, CodeFault{Rule: RuleDocNamesResolve, Key: doc + " " + line.Text, Path: doc,
					Reason: "the doc names " + command.Title + " (" + command.ID + "), which is not built: no handler file in the tree runs it"})
				continue
			}
			allowed := usageFlags(command.Usage)
			for _, flag := range line.Flags {
				if !allowed[flag] {
					faults = append(faults, CodeFault{Rule: RuleDocNamesResolve, Key: doc + " " + line.Text + " --" + flag, Path: doc,
						Reason: "the doc gives " + command.Title + " the flag --" + flag + ", which its usage (" + command.ID + ") does not list"})
				}
			}
		}
	}
	sortFaults(faults)
	return faults
}

// docNamesMissingPath is the predicate of ST-ASR-SP12 (ST-STD-DOCS-R02): a relative link of a user doc names a
// path that the tree does not hold, as a file or as a folder.
func docNamesMissingPath(spec Spec, tree Tree) []CodeFault {
	folders := make(map[string]bool)
	for _, name := range tree.paths() {
		for at := path.Dir(name); at != "." && !folders[at]; at = path.Dir(at) {
			folders[at] = true
		}
	}
	var faults []CodeFault
	for _, doc := range UserDocs(spec, tree) {
		data, ok := tree.File(doc)
		if !ok {
			continue
		}
		for _, match := range linkPattern.FindAllStringSubmatch(docProse(string(data)), -1) {
			target := match[1]
			if strings.Contains(target, "://") || strings.HasPrefix(target, "#") || strings.HasPrefix(target, "mailto:") {
				continue
			}
			target, _, _ = strings.Cut(target, "#")
			name := normalPath(path.Join(path.Dir(doc), target))
			if _, ok := tree.File(name); ok || folders[name] {
				continue
			}
			faults = append(faults, CodeFault{Rule: RuleDocNamesResolve, Key: doc + " " + match[1], Path: doc,
				Reason: "the doc links to " + match[1] + ", which the tree does not hold"})
		}
	}
	sortFaults(faults)
	return faults
}

// docNamedCommands are the CLI command records that the doc's prose names, by ID.
func docNamedCommands(commands []CLICommand, text string) map[string]bool {
	named := make(map[string]bool)
	for _, line := range docCommands(text, "stego") {
		if command, ok := matchCommand(commands, "stego", line.Words); ok {
			named[command.ID] = true
		}
	}
	return named
}

// docsLagChange is the check of ST-STD-DOCS-R04 that the change gate runs: for each user doc of the change's tree
// that the change leaves as it is at the base, each CLI command record that the doc's prose names at the base and
// whose usage or description the change alters, or that the change removes, is a reason, in doc then record order,
// written as codeFaultReason writes a fault, with the record's ID as its key.
func docsLagChange(baseSpec, spec Spec, baseTree, tree Tree) []string {
	after := make(map[string]CLICommand, len(spec.Commands))
	for _, command := range spec.Commands {
		after[command.ID] = command
	}
	var reasons []string
	for _, doc := range UserDocs(spec, tree) {
		before, atBase := baseTree.File(doc)
		now, held := tree.File(doc)
		if !atBase || !held || !bytes.Equal(before, now) {
			continue
		}
		named := docNamedCommands(baseSpec.Commands, string(before))
		for _, command := range baseSpec.Commands {
			if !named[command.ID] {
				continue
			}
			changed, kept := after[command.ID]
			switch {
			case !kept:
				reasons = append(reasons, doc+" ("+RuleDocFollowsChange+", "+command.ID+"): the doc names "+command.Title+", whose record "+command.ID+" the change removes, and the change does not change the doc")
			case changed.Usage != command.Usage || changed.Description != command.Description:
				reasons = append(reasons, doc+" ("+RuleDocFollowsChange+", "+command.ID+"): the doc names "+command.Title+", whose record "+command.ID+" the change alters, and the change does not change the doc")
			}
		}
	}
	return reasons
}
