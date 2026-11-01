// Command assure is the command line of the Assuring domain (ST-ASR). assure record records a proof run at the
// commit CI checked out (ST-ASR-CLI01), assure gate change gates a change against its base head (ST-ASR-CLI02),
// assure gate release gates a release commit on its own (ST-ASR-CLI03), and assure ledger shows what a commit
// proved (ST-ASR-CLI04).
package main

import (
	"fmt"
	"io"
	"os"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, os.Getenv, gitHubProvider))
}

// run runs one assure command and returns its exit code: 0 when it did its work or the commit passed, 1 when it
// refused or failed, and 2 when it was called wrongly or the gate cannot decide. open opens the provider's attestation store and signature check.
func run(args []string, stdout, stderr io.Writer, getenv func(string) string, open provider) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "record":
		return record(args[1:], stdout, stderr, getenv)
	case "gate":
		return gate(args[1:], stdout, stderr, getenv, open)
	case "ledger":
		return ledger(args[1:], stdout, stderr, getenv, open)
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	}
	fmt.Fprintf(stderr, "assure: unknown command %q\n", args[0])
	usage(stderr)
	return 2
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  assure record --commit <sha> --go-json <file> --python-json <file> --predicate <out>")
	fmt.Fprintln(w, "  assure gate change --commit <sha> --base <sha> [--lathe <path>]")
	fmt.Fprintln(w, "  assure gate release --commit <sha> --predicate <out> [--lathe <path>]")
	fmt.Fprintln(w, "  assure ledger --commit <sha> [--depth <n>] [--base <sha>]")
}
