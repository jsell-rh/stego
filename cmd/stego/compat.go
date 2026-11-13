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
	"strconv"
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
	if err == nil {
		err = refuseMovedService(base)
	}
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
// hold at that path is nil. Every other failure to read the commit or a file fails, so that an unreadable file is
// never taken as an absent one; a file that is no regular file, or that is larger than a declaration may be, is
// refused before it is read.
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
	folder := string(bytes.TrimSpace(prefix))
	for _, file := range []struct {
		name   string
		target *[]byte
	}{{"service.yaml", &base.Declaration}, {".stego/state.yaml", &base.Record}} {
		data, found, err := readBaseFile(base.Commit, folder+file.name)
		if err != nil {
			return base, fmt.Errorf("read %s at the base ref %s (commit %s): %w", folder+file.name, ref, base.Commit, err)
		}
		if found {
			*file.target = data
		}
	}
	return base, nil
}

// readBaseFile reads one file of a commit by its path from the root of the repository, and tells whether the
// commit holds it.
func readBaseFile(commit, name string) ([]byte, bool, error) {
	listing, err := compatGit("ls-tree", "-z", "--full-tree", commit, "--", name)
	if err != nil {
		return nil, false, err
	}
	if len(listing) == 0 {
		return nil, false, nil
	}
	entry, path, ok := strings.Cut(strings.TrimSuffix(string(listing), "\x00"), "\t")
	fields := strings.Fields(entry)
	if !ok || path != name || len(fields) != 3 || fields[1] != "blob" || (fields[0] != "100644" && fields[0] != "100755") {
		return nil, false, fmt.Errorf("the commit holds %s as %q, which is no regular file", name, entry)
	}
	size, err := compatGit("cat-file", "-s", fields[2])
	if err != nil {
		return nil, false, err
	}
	if n, err := strconv.ParseInt(strings.TrimSpace(string(size)), 10, 64); err != nil || n > parser.MaxDocumentBytes {
		return nil, false, fmt.Errorf("the file has %s bytes; at most %d are read", strings.TrimSpace(string(size)), parser.MaxDocumentBytes)
	}
	data, err := compatGit("cat-file", "blob", fields[2])
	if err != nil {
		return nil, false, err
	}
	return append([]byte{}, data...), true, nil
}

// refuseMovedService refuses a base that holds no service.yaml at the path of the service folder while a release of
// the service may have run: when the service in the working folder has a record, or when the base tree holds a
// record of a release at another path. The folder moved, or the base is not a version of this service, and the check
// cannot take an absent declaration as a first release.
func refuseMovedService(base compiler.BaseSource) error {
	if base.Declaration != nil {
		return nil
	}
	state, err := compiler.LoadState(".stego/state.yaml")
	if err != nil {
		return err
	}
	if state.LastApplied != nil {
		return fmt.Errorf("the base ref %s (commit %s) holds no service.yaml at the path of this service folder, and the service has a record, so a release of it has run: if the folder moved, run the check from the folder at the path that the base holds, or name a base that holds this folder", base.Ref, base.Commit)
	}
	released, err := releasedFolders(base.Commit)
	if err != nil {
		return fmt.Errorf("read the records of the base ref %s (commit %s): %w", base.Ref, base.Commit, err)
	}
	if len(released) > 0 {
		return fmt.Errorf("the base ref %s (commit %s) holds no service.yaml at the path of this service folder, and it holds the record of a release in %s: if the service folder moved, run the check from the folder at the path that the base holds", base.Ref, base.Commit, strings.Join(released, ", "))
	}
	return nil
}

// maxBaseRecords bounds the records that releasedFolders reads in one base tree.
const maxBaseRecords = 64

// releasedFolders gives each folder of the commit whose .stego/state.yaml records a release. A record that cannot
// be read counts as one.
func releasedFolders(commit string) ([]string, error) {
	listing, err := compatGit("ls-tree", "-r", "-z", "--name-only", "--full-tree", commit)
	if err != nil {
		return nil, err
	}
	var folders []string
	read := 0
	for _, name := range strings.Split(strings.TrimSuffix(string(listing), "\x00"), "\x00") {
		if name != ".stego/state.yaml" && !strings.HasSuffix(name, "/.stego/state.yaml") {
			continue
		}
		if read++; read > maxBaseRecords {
			return nil, fmt.Errorf("the commit holds more than %d records", maxBaseRecords)
		}
		data, found, err := readBaseFile(commit, name)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		if released, err := compiler.RecordsRelease(data, name); err != nil || released {
			folder := strings.TrimSuffix(strings.TrimSuffix(name, ".stego/state.yaml"), "/")
			if folder == "" {
				folder = "the root"
			}
			folders = append(folders, folder)
		}
	}
	return folders, nil
}

// compatGit runs one git command in the working folder with a time limit and gives its standard output. Git gets
// no GIT_ variable of the environment, so the caller's environment cannot point it at another repository or object
// store, and it follows no replace ref and fetches no missing object.
func compatGit(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), compatGitTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, "git", args...)
	var stdout, stderr bytes.Buffer
	var env []string
	for _, variable := range os.Environ() {
		if !strings.HasPrefix(variable, "GIT_") {
			env = append(env, variable)
		}
	}
	env = append(env, "GIT_TERMINAL_PROMPT=0", "GIT_NO_REPLACE_OBJECTS=1", "GIT_NO_LAZY_FETCH=1")
	command.Stdout, command.Stderr, command.Env = &stdout, &stderr, env
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}
