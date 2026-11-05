package main

import (
	"flag"
	"fmt"
	"io"

	"github.com/jsell-rh/stego/internal/registry"
)

// runRegistryConsumers implements `stego registry consumers <component>
// <checkout>...` (ST-SUP-CLI18). It reads the committed records of each
// checkout and prints every recorded service that uses the component, with
// the version it pins. It writes nothing.
func runRegistryConsumers(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("registry consumers", flag.ContinueOnError)
	flags.SetOutput(stderr)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() < 2 {
		return fmt.Errorf("usage: stego registry consumers <component> <checkout>...")
	}
	component := flags.Arg(0)
	reader, err := registry.ComponentConsumers(flags.Args()[1:])
	if err != nil {
		return err
	}
	uses := registry.ListConsumers(reader, component)
	if len(uses) == 0 {
		fmt.Fprintf(stdout, "No recorded service uses %s.\n", component)
		return nil
	}
	fmt.Fprintf(stdout, "Recorded services that use %s:\n", component)
	for _, use := range uses {
		fmt.Fprintf(stdout, "  %s %s (%s)\n", use.Service, use.Version, use.Checkout)
	}
	return nil
}
