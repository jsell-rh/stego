package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jsell-rh/stego/internal/fleet"
	"github.com/jsell-rh/stego/internal/lifecycle"
)

// runFleet parses stego fleet (ST-DIS-CLI06), stego fleet vex (ST-DIS-CLI09) and stego fleet measures
// (ST-DIS-CLI08), and reads the committed records of the named checkouts.
func runFleet(args []string) error {
	return runFleetTo(args, os.Stdout, time.Now())
}

func runFleetTo(args []string, stdout io.Writer, now time.Time) error {
	if len(args) > 0 {
		switch args[0] {
		case "vex":
			return runFleetVEX(args[1:], stdout, now)
		case "--help", "-h":
			printFleetUsage()
			return nil
		}
	}
	fs := flag.NewFlagSet("fleet", flag.ContinueOnError)
	digest := fs.String("digest", "", "the component digest to find")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *digest == "" {
		return errors.New("--digest is missing: name the component digest to find")
	}
	if fs.NArg() == 0 {
		return errors.New("name at least one service checkout to read")
	}
	reader := fleet.NewSweepReader(fleet.Project(fs.Args()))
	answer := fleet.FindByDigest(reader, *digest)
	pinning := map[string][]fleet.Hit{}
	for _, hit := range answer.Pinning {
		pinning[hit.Path] = append(pinning[hit.Path], hit)
	}
	notPinning := map[string]fleet.Service{}
	for _, service := range answer.NotPinning {
		notPinning[service.Path] = service
	}
	unknown := map[string]fleet.Unknown{}
	for _, u := range answer.Unknown {
		unknown[u.Path] = u
	}
	// One line for each checkout, in the order named: a sweep never leaves a checkout out.
	for _, path := range reader.Sweep().Order {
		for _, hit := range pinning[path] {
			fmt.Fprintf(stdout, "pins %s %s component %s version %s digest %s\n", hit.Service, path, hit.Component, hit.Version, hit.Digest)
		}
		if service, held := notPinning[path]; held {
			fmt.Fprintf(stdout, "does-not-pin %s %s\n", service.Name, path)
		}
		if u, held := unknown[path]; held {
			if u.FormatVersion != 0 {
				fmt.Fprintf(stdout, "unknown %s format-version %d: %s\n", path, u.FormatVersion, u.Reason)
			} else {
				fmt.Fprintf(stdout, "unknown %s: %s\n", path, u.Reason)
			}
		}
	}
	return nil
}

func runFleetVEX(args []string, stdout io.Writer, now time.Time) error {
	fs := flag.NewFlagSet("fleet vex", flag.ContinueOnError)
	registryDir := fs.String("registry", "", "the registry checkout whose lifecycle folder holds the advisories")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *registryDir == "" {
		return errors.New("--registry is missing: name the registry checkout that holds the advisories")
	}
	if fs.NArg() == 0 {
		return errors.New("name at least one service checkout to read")
	}
	lifecycles, err := lifecycle.NewStore(*registryDir).All()
	if err != nil {
		return fmt.Errorf("the advisories of the registry cannot be read: %w", err)
	}
	document := fleet.VEX(fleet.NewSweepReader(fleet.Project(fs.Args())), lifecycles, now)
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(document)
}

func printFleetUsage() {
	fmt.Fprintln(os.Stderr, "Usage: stego fleet --digest <digest> <checkout>...")
	fmt.Fprintln(os.Stderr, "       stego fleet vex --registry <folder> <checkout>...")
}
