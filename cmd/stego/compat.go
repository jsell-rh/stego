package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/jsell-rh/stego/internal/compiler"
	"github.com/jsell-rh/stego/internal/parser"
)

// exitStatus is an error that ends stego with its own exit status, after the command printed what it found.
type exitStatus struct {
	code   int
	reason string
}

func (e exitStatus) Error() string { return e.reason }

// compatUsage is the exit status of stego compat for a usage error or a base ref that it cannot read.
const compatUsage = 2

// compatGitTimeout bounds each git command that stego compat runs.
const compatGitTimeout = time.Minute

// runCompat implements stego compat (ST-GEN-CLI07): it reads the declaration and the record at the base ref from
// the service's repository, checks the head declaration in the working folder against them (ST-GEN-QRY04), and
// prints the base ref and its commit and each fault. It exits 0 with no fault, 1 with a fault, and 2 for a usage
// error or a ref that it cannot read. It writes nothing.
func runCompat(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("compat", flag.ContinueOnError)
	baseRef := fs.String("base", "", "the commit or branch of the version that runs now")
	structured := fs.Bool("json", false, "print the base and the faults as one JSON object")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return exitStatus{compatUsage, err.Error()}
	}
	if fs.NArg() != 0 || *baseRef == "" {
		return exitStatus{compatUsage, "compat takes --base <ref> and no argument: stego compat --base <ref> [--json]"}
	}
	if strings.HasPrefix(*baseRef, "-") {
		return exitStatus{compatUsage, fmt.Sprintf("the base ref %q starts with -, which git reads as an option", *baseRef)}
	}
	base, err := readBase(*baseRef)
	if err != nil {
		return exitStatus{compatUsage, err.Error()}
	}
	head, err := parser.ParseServiceDeclaration("service.yaml")
	if err != nil {
		return err
	}
	check, err := compiler.CheckBase(base, head)
	if err != nil {
		return err
	}
	if *structured {
		document, err := json.MarshalIndent(check, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(stdout, string(document))
	} else {
		fmt.Fprint(stdout, formatBaseCheck(check))
	}
	if check.HasFaults() {
		return exitStatus{1, fmt.Sprintf("the head declaration has %d fault(s) against the base ref %s", len(check.Faults), check.BaseRef)}
	}
	return nil
}

// formatBaseCheck gives the check as text: the base it took, then each fault with its entity, field, rule and the
// schema changes to declare.
func formatBaseCheck(check compiler.BaseCheck) string {
	var out strings.Builder
	fmt.Fprintf(&out, "Base: %s at commit %s, taken as the version that runs now.\n", check.BaseRef, check.BaseCommit)
	if !check.Released {
		out.WriteString("No fault: the base holds no record, so no release of the service has run, and any stored shape is accepted.\n")
		return out.String()
	}
	if !check.HasFaults() {
		out.WriteString("No fault: the head can run beside the base on a database with rows.\n")
		return out.String()
	}
	for _, fault := range check.Faults {
		where := fault.Entity
		if fault.Field != "" {
			where += "." + fault.Field
		}
		if where == "" {
			where = "declaration"
		}
		fmt.Fprintf(&out, "fault %s %s: %s\n", fault.Rule, where, fault.Message)
		for _, change := range fault.Declare {
			fmt.Fprintf(&out, "  declare: %s\n", change)
		}
	}
	return out.String()
}

// readBase reads the base ref from the Git repository that holds the working folder: the commit it resolves to,
// and service.yaml and .stego/state.yaml of the service folder at that commit. A file that the commit does not
// hold is nil.
func readBase(ref string) (compiler.BaseSource, error) {
	base := compiler.BaseSource{Ref: ref}
	prefix, err := compatGit("rev-parse", "--show-prefix")
	if err != nil {
		return base, fmt.Errorf("the working folder is not in a Git repository, so the base ref %s cannot be read: %w", ref, err)
	}
	commit, err := compatGit("rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{commit}")
	if err != nil {
		return base, fmt.Errorf("the base ref %s names no commit of the service's repository", ref)
	}
	base.Commit = strings.TrimSpace(string(commit))
	prefix = bytes.TrimSpace(prefix)
	for _, file := range []struct {
		name   string
		target *[]byte
	}{{"service.yaml", &base.Declaration}, {".stego/state.yaml", &base.Record}} {
		object := base.Commit + ":" + string(prefix) + file.name
		if _, err := compatGit("cat-file", "-e", object); err != nil {
			continue
		}
		data, err := compatGit("cat-file", "blob", object)
		if err != nil {
			return base, fmt.Errorf("read %s at the base ref %s: %w", file.name, ref, err)
		}
		*file.target = append([]byte{}, data...)
	}
	return base, nil
}

// compatGit runs one git command in the working folder with a time limit and gives its standard output.
func compatGit(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), compatGitTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, "git", args...)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr, command.Env = &stdout, &stderr, append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}
