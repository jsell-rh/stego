// Command assure is the command line of the Assuring domain (ST-ASR). assure record records a proof run at the
// commit CI checked out (ST-ASR-CLI01).
package main

import (
	"fmt"
	"io"
	"os"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, os.Getenv))
}

// run runs one assure command and returns its exit code: 0 when it did its work, 1 when it refused or failed,
// and 2 when it was called wrongly.
func run(args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "record":
		return record(args[1:], stdout, stderr, getenv)
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
}
