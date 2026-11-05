package main

import (
	"flag"
	"fmt"

	"github.com/jsell-rh/stego/internal/browserassets"
	"github.com/jsell-rh/stego/internal/compiler"
)

func runAssets(args []string) error {
	flags := flag.NewFlagSet("assets", flag.ContinueOnError)
	directory := flags.String("directory", "", "directory with index.html and assets")
	output := flags.String("output", "", "output ZIP file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *directory == "" || *output == "" {
		return fmt.Errorf("assets requires --directory and --output")
	}
	data, err := compiler.PackBrowserAssets(*directory)
	if err != nil {
		return err
	}
	assets, err := browserassets.Decode(data)
	if err != nil {
		return err
	}
	names := map[string]bool{}
	var index []byte
	for _, asset := range assets {
		names[asset.Path] = true
		if asset.Path == "index.html" {
			index = asset.Data
		}
	}
	if _, err := browserassets.ScriptHashes(index, names); err != nil {
		return fmt.Errorf("browser asset index.html is refused: %w", err)
	}
	return compiler.WriteBrowserAssets(*output, data)
}
