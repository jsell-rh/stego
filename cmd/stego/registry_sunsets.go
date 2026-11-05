package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jsell-rh/stego/internal/registry"
)

// runRegistrySunsets implements `stego registry sunsets <checkout>...`
// (ST-SUP-CLI04). It composes the project's active registry, reads the
// committed records of each checkout, and prints every recorded service that
// uses a component past its sunset, with the component and the date. It
// fails when there is any.
func runRegistrySunsets(args []string, stdout, stderr io.Writer, today time.Time) error {
	flags := flag.NewFlagSet("registry sunsets", flag.ContinueOnError)
	flags.SetOutput(stderr)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() < 1 {
		return fmt.Errorf("usage: stego registry sunsets <checkout>...")
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
	reader, err := registry.ComponentConsumers(flags.Args())
	if err != nil {
		return err
	}
	missed := registry.MissedSunsets(reader, active, today)
	if len(missed) == 0 {
		fmt.Fprintln(stdout, "No recorded service uses a component past its sunset.")
		return nil
	}
	fmt.Fprintln(stdout, "Recorded services that use a component past its sunset:")
	for _, use := range missed {
		fmt.Fprintf(stdout, "  %s uses %s %s (%s), past its sunset %s\n", use.Service, use.Component, use.Version, use.Stage, use.Sunset)
	}
	return fmt.Errorf("%d recorded services use a component past its sunset", len(missed))
}
