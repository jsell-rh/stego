package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/jsell-rh/stego/internal/registry"
)

// runRegistryCompose implements `stego registry compose` (ST-SUP-CLI09). It
// reads the registry sources that the project pins in .stego/config.yaml,
// resolves each at its pinned commit, composes them into one active set and
// prints each source with its reference and the parts it adds. It refuses a
// composition that registry.ResolveRegistry or registry.LoadSources refuses.
func runRegistryCompose(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("registry compose", flag.ContinueOnError)
	flags.SetOutput(stderr)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("usage: stego registry compose")
	}
	projectDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting working directory: %w", err)
	}
	resolved, err := registry.ResolveRegistry(registry.ResolveOptions{ProjectDir: projectDir, Stderr: stderr})
	if err != nil {
		return err
	}
	active, err := registry.LoadSources(resolved.Sources...)
	if err != nil {
		return err
	}
	sources := active.Sources()
	fmt.Fprintf(stdout, "Active registry: %d sources, content digest %s\n", len(sources), active.ContentHash())
	for _, source := range sources {
		fmt.Fprintf(stdout, "%s registry %s\n", source.Trust, source.Name)
		if len(source.Parts) == 0 {
			fmt.Fprintln(stdout, "  (adds no part)")
		}
		for _, part := range source.Parts {
			fmt.Fprintf(stdout, "  %s\n", part)
		}
	}
	return nil
}
