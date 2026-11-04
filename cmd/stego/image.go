package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/jsell-rh/stego/internal/appbuild"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func runImage(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("image requires build, sign, verify, export, publish, or retrieve")
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
		after := flags.String("after", "", "Application built event that stego build printed (ST-BLD-PO01)")
		result := flags.String("build-result", "", "result folder of stego build, with --after")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return fmt.Errorf("image build accepts no positional arguments")
		}
		if *after != "" || *result != "" {
			if *after == "" || *result == "" {
				return fmt.Errorf("image build requires --after and --build-result together")
			}
			var event appbuild.ApplicationBuilt
			if err := readEvent(*after, &event); err != nil {
				return err
			}
			_, raised, err := appbuild.BuildImageOnApplicationBuilt(ctx, event, *result, options)
			if err != nil {
				return err
			}
			return printEvent(raised)
		}
		record, err := appbuild.BuildImage(ctx, options)
		if err != nil {
			return err
		}
		return printEvent(appbuild.ImageBuilt{ImageDigest: record.Manifest.SHA256, BuildRecordDigest: record.BuildRecordSHA256})
	case "sign":
		return runImageSign(ctx, args[1:])
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
		check := signatureFlags(flags)
		var after *string
		if args[0] == "publish" {
			after = flags.String("after", "", "Image signed event that stego image sign printed (ST-BLD-PO03); --image is then the image folder")
		}
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return fmt.Errorf("registry image commands accept no positional arguments")
		}
		if args[0] == "publish" {
			if *after == "" {
				if signed := signatureFlagSet(flags); signed != "" {
					return fmt.Errorf("image publish takes --%s only with --after", signed)
				}
				published, err := appbuild.PublishImage(ctx, options)
				if err != nil {
					return err
				}
				return printEvent(appbuild.ImagePublished{ImageDigest: strings.TrimPrefix(published.Reference, published.Repository+"@sha256:"), Repository: published.Repository})
			}
			var event appbuild.ImageSigned
			if err := readEvent(*after, &event); err != nil {
				return err
			}
			signature, err := check()
			if err != nil {
				return err
			}
			image := options.Image
			options.Image = ""
			options.Signature = signature
			_, raised, err := appbuild.PublishImageOnImageSigned(ctx, event, image, options)
			if err != nil {
				return err
			}
			return printEvent(raised)
		}
		if options.Image != "" {
			return fmt.Errorf("image retrieve does not accept an input image directory")
		}
		signature, err := check()
		if err != nil {
			return err
		}
		options.Signature = signature
		_, err = appbuild.RetrieveImage(ctx, options)
		return err
	case "verify", "export":
		record := flags.String("record", "", "image record")
		expected := flags.String("record-sha256", "", "trusted image record digest")
		image := flags.String("image", "", "OCI image directory")
		build := flags.String("build-record", "", "application build record")
		work := flags.String("work", "", "new private verification directory")
		var check func() (appbuild.SignatureCheck, error)
		if args[0] == "verify" {
			check = signatureFlags(flags)
		}
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
		signature, err := check()
		if err != nil {
			return err
		}
		_, err = appbuild.VerifyImage(ctx, appbuild.ImageVerifyOptions{Record: *record, RecordSHA256: *expected, Image: *image, BuildRecord: *build, Work: *work, Check: signature})
		return err
	default:
		return fmt.Errorf("image requires build, sign, verify, export, publish, or retrieve")
	}
}

// readEvent reads the event that an earlier stego run printed, as one JSON
// object of at most 64 KiB with no unknown field and nothing after it.
func readEvent(name string, event any) error {
	file, err := os.Open(name)
	if err != nil {
		return fmt.Errorf("cannot read the event %s: %w", name, err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 64<<10+1))
	if err != nil || len(data) > 64<<10 {
		return fmt.Errorf("cannot read the event %s as a file of at most 64 KiB", name)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(event); err != nil {
		return fmt.Errorf("the event %s is not valid: %w", name, err)
	}
	if decoder.More() {
		return fmt.Errorf("the event %s holds more than one event", name)
	}
	return nil
}

// printEvent prints the event that a command raises, as one line of JSON, so
// that the next stego run can hear it with --after.
func printEvent(event any) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	_, err = fmt.Println(string(data))
	return err
}
