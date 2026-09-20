package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/jsell-rh/stego/internal/appbuild"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func runImage(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("image requires build or verify")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	flags := flag.NewFlagSet("image "+args[0], flag.ContinueOnError)
	switch args[0] {
	case "build":
		var options appbuild.ImageOptions
		flags.StringVar(&options.BuildRecord, "build-record", "", "verified application build record")
		flags.StringVar(&options.BuildRecordSHA256, "build-record-sha256", "", "trusted application record digest")
		flags.StringVar(&options.Application, "application", "", "application executable")
		flags.StringVar(&options.TrustStore, "trust-store", "", "explicit PEM CA bundle")
		flags.StringVar(&options.TrustStoreSHA256, "trust-store-sha256", "", "trusted CA bundle digest")
		flags.StringVar(&options.Entrypoint, "entrypoint", "application", "image executable name")
		flags.StringVar(&options.Output, "output", "", "new private output directory")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return fmt.Errorf("image build accepts no positional arguments")
		}
		_, err := appbuild.BuildImage(ctx, options)
		return err
	case "verify":
		record := flags.String("record", "", "image record")
		expected := flags.String("record-sha256", "", "trusted image record digest")
		image := flags.String("image", "", "OCI image directory")
		build := flags.String("build-record", "", "application build record")
		work := flags.String("work", "", "new private verification directory")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return fmt.Errorf("image verify accepts no positional arguments")
		}
		_, err := appbuild.VerifyImage(ctx, *record, *image, *build, *expected, *work)
		return err
	default:
		return fmt.Errorf("image requires build or verify")
	}
}
