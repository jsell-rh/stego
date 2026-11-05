package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/jsell-rh/stego/internal/compiler"
	"github.com/jsell-rh/stego/internal/fill"
)

// runFillStatus implements `stego fill status` (ST-EXT-CLI04): it prints
// each fill that the declaration binds, with its qualification standing, its
// fit, and why it is not qualified or does not fit. It writes nothing.
func runFillStatus(args []string) error {
	fs := flag.NewFlagSet("fill status", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("fill status takes no arguments")
	}
	input, err := buildReconcilerInput()
	if err != nil {
		return err
	}
	check, err := compiler.ResolveFillCheck(input)
	if err != nil {
		return err
	}
	rows, err := fill.ListQualification(check)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Println("The declaration binds no fill.")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "FILL\tSTANDING\tFITS\tREASON\n")
	for _, row := range rows {
		fits := "yes"
		if !row.Fits {
			fits = "no"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", row.Fill, row.Standing, fits, strings.Join(row.Reasons, "; "))
	}
	return w.Flush()
}
