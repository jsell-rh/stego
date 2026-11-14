package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/jsell-rh/stego/internal/appbuild"
)

func runBuild(args []string) error {
	if len(args) > 0 && args[0] == "verify-source" {
		flags := flag.NewFlagSet("build verify-source", flag.ContinueOnError)
		record := flags.String("record", "", "build record file")
		expected := flags.String("record-sha256", "", "build record digest from verified provenance")
		source := flags.String("source", "", "complete source snapshot without Git metadata or build caches")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 || *record == "" || *expected == "" || *source == "" {
			return fmt.Errorf("build verify-source requires --record, --record-sha256, and --source")
		}
		_, err := appbuild.VerifySource(*record, *expected, *source)
		return err
	}
	if len(args) > 0 && args[0] == "download" {
		flags := flag.NewFlagSet("build download", flag.ContinueOnError)
		var options appbuild.DownloadOptions
		flags.StringVar(&options.Source, "source", "", "Git source repository")
		flags.StringVar(&options.Revision, "revision", "", "full source commit ID")
		flags.StringVar(&options.Module, "module", ".", "module path within the source repository")
		flags.StringVar(&options.Target, "target", "", "entry point path within the module")
		flags.StringVar(&options.Go, "go", "", "bin/go in the selected official SDK")
		flags.StringVar(&options.Output, "output", "", "new module cache directory")
		flags.StringVar(&options.RuntimeFolder, "runtime-folder", "", "registry/runtime of a STEGO checkout, for a source with no runtime folder; it must hash to the runtime pin")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 || options.Source == "" || options.Revision == "" || options.Target == "" || options.Go == "" || options.Output == "" {
			return fmt.Errorf("build download requires --source, --revision, --target, --go, and --output")
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		_, err := appbuild.Download(ctx, options)
		return err
	}
	if len(args) > 0 && args[0] == "verify" {
		flags := flag.NewFlagSet("build verify", flag.ContinueOnError)
		record := flags.String("record", "", "build record file")
		artifact := flags.String("artifact", "", "application executable")
		expected := flags.String("record-sha256", "", "build record digest from verified provenance")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 || *record == "" || *artifact == "" || *expected == "" {
			return fmt.Errorf("build verify requires --record, --artifact, and --record-sha256")
		}
		_, err := appbuild.Verify(*record, *artifact, *expected)
		return err
	}
	flags := flag.NewFlagSet("build", flag.ContinueOnError)
	var options appbuild.Options
	flags.StringVar(&options.Source, "source", "", "Git source repository")
	flags.StringVar(&options.Revision, "revision", "", "full source commit ID")
	flags.StringVar(&options.Module, "module", ".", "module path within the source repository")
	flags.StringVar(&options.Target, "target", "", "entry point path within the module")
	flags.StringVar(&options.Go, "go", "", "bin/go in the selected official SDK")
	flags.StringVar(&options.Work, "work", "", "new private build directory")
	flags.StringVar(&options.Output, "output", "", "new result directory")
	flags.StringVar(&options.ModuleCache, "module-cache", "", "module cache that stego build download recorded; required, since a build runs offline")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("build accepts no positional arguments")
	}
	if options.ModuleCache == "" {
		return fmt.Errorf("build requires --module-cache: run stego build download first; a build never downloads modules")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if _, err := appbuild.Build(ctx, options); err != nil {
		return err
	}
	// The build raises Application built (ST-BLD-EV03); stego image build
	// hears it with --after (ST-BLD-PO01).
	event, err := appbuild.ApplicationBuiltIn(options.Output)
	if err != nil {
		return err
	}
	return printEvent(event)
}
