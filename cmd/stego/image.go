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
		return fmt.Errorf("image requires build, verify, export, publish, or retrieve")
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
	case "publish", "retrieve":
		var options appbuild.RegistryImageOptions
		flags.StringVar(&options.Record, "record", "", "authenticated image record")
		flags.StringVar(&options.RecordSHA256, "record-sha256", "", "authenticated image record digest")
		flags.StringVar(&options.BuildRecord, "build-record", "", "application build record")
		flags.StringVar(&options.Image, "image", "", "input OCI image directory for publication")
		flags.StringVar(&options.Work, "work", "", "new private result directory")
		flags.StringVar(&options.Access.Repository, "repository", "", "explicit registry repository without a tag")
		flags.StringVar(&options.Access.CAFile, "registry-ca", "", "explicit registry PEM CA bundle")
		flags.StringVar(&options.Access.CASHA256, "registry-ca-sha256", "", "trusted registry CA bundle digest")
		flags.StringVar(&options.Access.Credentials, "credentials", "", "private canonical username and password JSON file")
		flags.Func("token-origin", "approved HTTPS token origin; repeat for each origin", func(value string) error {
			options.Access.TokenOrigins = append(options.Access.TokenOrigins, value)
			return nil
		})
		flags.Func("blob-origin", "approved HTTPS blob origin; repeat for each origin", func(value string) error {
			options.Access.BlobOrigins = append(options.Access.BlobOrigins, value)
			return nil
		})
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return fmt.Errorf("registry image commands accept no positional arguments")
		}
		if args[0] == "publish" {
			_, err := appbuild.PublishImage(ctx, options)
			return err
		}
		if options.Image != "" {
			return fmt.Errorf("image retrieve does not accept an input image directory")
		}
		_, err := appbuild.RetrieveImage(ctx, options)
		return err
	case "verify", "export":
		record := flags.String("record", "", "image record")
		expected := flags.String("record-sha256", "", "trusted image record digest")
		image := flags.String("image", "", "OCI image directory")
		build := flags.String("build-record", "", "application build record")
		work := flags.String("work", "", "new private verification directory")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return fmt.Errorf("image verification accepts no positional arguments")
		}
		if args[0] == "export" {
			_, err := appbuild.ExportImage(ctx, *record, *image, *build, *expected, *work)
			return err
		}
		_, err := appbuild.VerifyImage(ctx, *record, *image, *build, *expected, *work)
		return err
	default:
		return fmt.Errorf("image requires build, verify, export, publish, or retrieve")
	}
}
