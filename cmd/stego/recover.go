package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/jsell-rh/stego/internal/compiler"
)

func runRecover(args []string) error {
	flags := flag.NewFlagSet("recover", flag.ContinueOnError)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("recover takes no arguments")
	}
	project, err := os.Getwd()
	if err != nil {
		return err
	}
	if err := compiler.Recover(project); err != nil {
		return err
	}
	fmt.Println("Recovery complete. No pending apply remains.")
	return nil
}
