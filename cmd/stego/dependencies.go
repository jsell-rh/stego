package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"

	"github.com/jsell-rh/stego/internal/compiler"
)

func runDependencies(args []string) error {
	flags := flag.NewFlagSet("deps", flag.ContinueOnError)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("stego deps takes no arguments")
	}
	input, err := buildReconcilerInput()
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := compiler.ResolveDependencies(ctx, input); err != nil {
		return err
	}
	fmt.Println("Dependencies resolved. Commit go.mod and go.sum with the application changes.")
	return nil
}
